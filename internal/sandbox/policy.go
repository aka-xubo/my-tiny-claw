package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/aka-xubo/my-tiny-claw/internal/sandbox/proxy"
)

type ApprovalPolicy string

const (
	ApprovalUntrusted ApprovalPolicy = "untrusted"
	ApprovalOnRequest ApprovalPolicy = "on-request"
	ApprovalNever     ApprovalPolicy = "never"
)

type SandboxPermission string

const (
	UseDefault                SandboxPermission = "use_default"
	WithAdditionalPermissions SandboxPermission = "with_additional_permissions"
	RequireEscalated          SandboxPermission = "require_escalated"
)

type AdditionalPermissions struct {
	Filesystem map[string]Access
	Network    NetworkMode
}

type CallPermissions struct {
	Tool                  string
	Command               string
	Dir                   string
	Operation             string
	Path                  string
	SandboxPermissions    SandboxPermission
	Justification         string
	AdditionalPermissions *AdditionalPermissions
}

type Session struct {
	proxyMu      sync.Mutex
	mu           sync.Mutex
	profile      *Profile
	policy       ApprovalPolicy
	approver     Approver
	cache        approvalCache
	roots        []string
	envExclude   []string
	envKeep      []string
	proxyServers map[string]*proxy.Server
	proxySources map[string]map[string]struct{}
	ctx          context.Context
	cancel       context.CancelFunc
	closed       bool
}

type approvalCache struct {
	commands    map[string]struct{}
	writeOps    map[string]struct{}
	escalations map[string]struct{}
	network     map[string]struct{}
	additional  []AdditionalPermissions
}

func NewSession(profile *Profile, policy ApprovalPolicy, approver Approver, roots []string) (*Session, error) {
	if profile == nil {
		return nil, errors.New("权限档不能为空")
	}
	if policy == "" {
		policy = ApprovalOnRequest
	}
	if policy != ApprovalUntrusted && policy != ApprovalOnRequest && policy != ApprovalNever {
		return nil, fmt.Errorf("未知审批策略: %s", policy)
	}
	if approver == nil {
		approver = DenyApprover{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Session{
		ctx: ctx, cancel: cancel,
		profile:  cloneProfile(profile),
		policy:   policy,
		approver: approver,
		cache: approvalCache{
			commands:    make(map[string]struct{}),
			writeOps:    make(map[string]struct{}),
			escalations: make(map[string]struct{}),
			network:     make(map[string]struct{}),
		},
		roots:        append([]string(nil), roots...),
		proxyServers: make(map[string]*proxy.Server),
		proxySources: make(map[string]map[string]struct{}),
	}, nil
}

func (s *Session) Profile() *Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneProfile(s.profile)
}

func (s *Session) Description() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.profile == nil {
		return "当前沙箱状态不可用。"
	}
	writable := make([]string, 0)
	protected := make([]string, 0)
	for _, rule := range s.profile.Files {
		if rule.Access == AccessWrite {
			writable = append(writable, rule.Path)
		} else if rule.Path != "/" {
			label := "read"
			if rule.Access == AccessDeny {
				label = "deny"
			}
			path := rule.Path
			if rule.Glob != "" {
				path += "/" + rule.Glob
			}
			protected = append(protected, path+"="+label)
		}
	}
	return fmt.Sprintf("当前权限档：%s\n可写路径：%s\n受保护规则：%s\n网络模式：%s\n审批策略：%s\n工具提权参数：use_default、with_additional_permissions、require_escalated。", s.profile.Name, strings.Join(writable, ", "), strings.Join(protected, ", "), networkModeName(s.profile.Network), s.policy)
}

func networkModeName(mode NetworkMode) string {
	switch mode {
	case NetworkProxy:
		return "proxy"
	case NetworkFull:
		return "full"
	default:
		return "disabled"
	}
}

func (s *Session) SetProfile(profile *Profile) error {
	if profile == nil {
		return errors.New("权限档不能为空")
	}
	s.mu.Lock()
	s.profile = cloneProfile(profile)
	s.mu.Unlock()
	return nil
}

func (s *Session) Policy() ApprovalPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy
}

func (s *Session) SetPolicy(policy ApprovalPolicy) error {
	if policy != ApprovalUntrusted && policy != ApprovalOnRequest && policy != ApprovalNever {
		return fmt.Errorf("未知审批策略: %s", policy)
	}
	s.mu.Lock()
	s.policy = policy
	s.mu.Unlock()
	return nil
}

func (s *Session) SetEnvironmentPolicy(exclude, keep []string) {
	s.mu.Lock()
	s.envExclude = append([]string(nil), exclude...)
	s.envKeep = append([]string(nil), keep...)
	s.mu.Unlock()
}

func (s *Session) Close() error {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	s.mu.Lock()
	s.closed = true
	s.cancel()
	servers := make([]*proxy.Server, 0, len(s.proxyServers))
	for _, server := range s.proxyServers {
		servers = append(servers, server)
	}
	s.proxyServers = make(map[string]*proxy.Server)
	s.mu.Unlock()
	var first error
	for _, server := range servers {
		if err := server.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Session) Context() context.Context { return s.ctx }

// Spawn applies the same environment cleanup and platform backend to every
// child process created by this session.
func (s *Session) Spawn(ctx context.Context, argv []string, dir string, profile *Profile) (*exec.Cmd, func(), error) {
	if profile == nil {
		profile = s.Profile()
	}
	if runtime.GOOS != "darwin" && !profile.FullAccess {
		return nil, nil, ErrUnsupported
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, errors.New("沙箱会话已关闭")
	}
	roots := append([]string(nil), s.roots...)
	envExclude := append([]string(nil), s.envExclude...)
	envKeep := append([]string(nil), s.envKeep...)
	s.mu.Unlock()
	var endpoints *ProxyEndpoints
	if profile.Network == NetworkProxy && !profile.FullAccess {
		_, proxyEndpoints, err := s.proxyForProfile(profile)
		if err != nil {
			return nil, nil, err
		}
		endpoints = &proxyEndpoints
		s.RecordProxySource(profile, strings.Join(argv, " "), dir, "")
	}
	return Prepare(ctx, SpawnRequest{
		Argv:          argv,
		Dir:           dir,
		Env:           os.Environ(),
		Profile:       profile,
		WritableRoots: roots,
		EnvExclude:    envExclude,
		EnvKeep:       envKeep,
		Proxy:         endpoints,
	})
}

func (s *Session) proxyForProfile(profile *Profile) (*proxy.Server, ProxyEndpoints, error) {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	rules := make(proxy.Rules, len(profile.Domains))
	for k, v := range profile.Domains {
		rules[k] = proxy.DomainRule(v)
	}
	normalized, err := proxy.NormalizeRules(rules)
	if err != nil {
		return nil, ProxyEndpoints{}, err
	}
	profile = cloneProfile(profile)
	profile.Domains = make(map[string]DomainRule, len(normalized))
	for k, v := range normalized {
		profile.Domains[k] = DomainRule(v)
	}
	key := proxyProfileKey(profile)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ProxyEndpoints{}, errors.New("沙箱会话已关闭")
	}
	if server, ok := s.proxyServers[key]; ok {
		s.mu.Unlock()
		return server, proxyEndpoints(server), nil
	}
	s.mu.Unlock()
	server := &proxy.Server{Rules: normalized}
	server.Approve = func(ctx context.Context, host string, port int) (proxy.Decision, error) {
		s.mu.Lock()
		sources := []string{}
		for source := range s.proxySources[key] {
			sources = append(sources, source)
		}
		s.mu.Unlock()
		sort.Strings(sources)
		return s.approveNetwork(ctx, host, port, s.Policy(), sources...)
	}
	endpoints, err := server.Start()
	if err != nil {
		return nil, ProxyEndpoints{}, fmt.Errorf("启动网络代理失败: %w", err)
	}
	s.mu.Lock()
	s.proxyServers[key] = server
	s.mu.Unlock()
	return server, ProxyEndpoints{HTTPPort: endpoints.HTTPPort, SOCKSPort: endpoints.SOCKSPort}, nil
}

func proxyEndpoints(server *proxy.Server) ProxyEndpoints {
	result := ProxyEndpoints{}
	if address := server.HTTPAddr; address != "" {
		if parsed, err := net.ResolveTCPAddr("tcp", address); err == nil {
			result.HTTPPort = parsed.Port
		}
	}
	if address := server.SOCKSAddr; address != "" {
		if parsed, err := net.ResolveTCPAddr("tcp", address); err == nil {
			result.SOCKSPort = parsed.Port
		}
	}
	return result
}

func (s *Session) approveNetwork(ctx context.Context, host string, port int, policy ApprovalPolicy, sources ...string) (proxy.Decision, error) {
	normalized, _, err := proxy.NormalizeHost(host)
	if err != nil {
		return proxy.Deny, err
	}
	s.mu.Lock()
	_, cached := s.cache.network[normalized]
	s.mu.Unlock()
	if policy == ApprovalNever {
		return proxy.Deny, nil
	}
	if cached {
		return proxy.Allow, nil
	}
	result, err := s.approve(ctx, ApprovalRequest{Kind: ApprovalNetwork, Host: normalized, Port: port, Sources: sources})
	if err != nil || result.Decision == Deny {
		return proxy.Deny, err
	}
	if result.Decision == AllowSession {
		s.mu.Lock()
		s.cache.network[normalized] = struct{}{}
		s.mu.Unlock()
	}
	return proxy.Allow, nil
}

func proxyProfileKey(profile *Profile) string {
	rules := proxy.Rules{}
	for k, v := range profile.Domains {
		rules[k] = proxy.DomainRule(v)
	}
	normalized, err := proxy.NormalizeRules(rules)
	if err == nil {
		profile = cloneProfile(profile)
		profile.Domains = map[string]DomainRule{}
		for k, v := range normalized {
			profile.Domains[k] = DomainRule(v)
		}
	}
	keys := make([]string, 0, len(profile.Domains))
	for domain, rule := range profile.Domains {
		keys = append(keys, domain+"="+string(rule))
	}
	sort.Strings(keys)
	return strings.Join(keys, "\x00")
}

// A shared proxy cannot authenticate the individual originating process.
// Keep candidate commands and background IDs for the approval prompt.
func (s *Session) RecordProxySource(profile *Profile, command, dir, id string) {
	if profile.Network != NetworkProxy || profile.FullAccess {
		return
	}
	key := proxyProfileKey(profile)
	label := dir + ": " + command
	if id != "" {
		label = "会话 " + id + " · " + label
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proxySources[key] == nil {
		s.proxySources[key] = map[string]struct{}{}
	}
	s.proxySources[key][label] = struct{}{}
}

func (s *Session) Authorize(ctx context.Context, call CallPermissions) (*Profile, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	base := cloneProfile(s.profile)
	policy := s.policy
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, errors.New("沙箱会话已关闭")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if call.Dir != "" {
		dir, err := NormalizePath(call.Dir)
		if err != nil {
			return nil, err
		}
		call.Dir = dir
	}
	if call.Path != "" {
		if !filepath.IsAbs(call.Path) {
			call.Path = filepath.Join(call.Dir, call.Path)
		}
		path, err := NormalizePath(call.Path)
		if err != nil {
			return nil, err
		}
		call.Path = path
	}
	if call.Command == "" && call.Path != "" {
		call.Command = call.Operation + " " + call.Path
	}
	if base == nil {
		return nil, errors.New("当前会话没有权限档")
	}
	request := call.SandboxPermissions
	if request == "" {
		request = UseDefault
	}
	if base.FullAccess {
		request = UseDefault
	}

	switch request {
	case UseDefault:
		if policy == ApprovalUntrusted && requiresUntrustedApproval(call) {
			key := untrustedCacheKey(call)
			if !s.hasUntrustedCache(key, call) {
				result, err := s.approve(ctx, ApprovalRequest{
					Kind:    approvalKindForCall(call),
					Tool:    call.Tool,
					Command: call.Command,
					Dir:     call.Dir,
				})
				if err != nil || result.Decision == Deny {
					return nil, deniedError(result, err)
				}
				if result.Decision == AllowSession {
					s.addCommandCache(key, call)
				}
			}
		}
		return base, nil

	case WithAdditionalPermissions:
		if policy == ApprovalNever {
			return nil, errors.New("当前审批策略 never 拒绝额外权限申请")
		}
		if call.AdditionalPermissions == nil {
			return nil, errors.New("with_additional_permissions 必须提供 additional_permissions")
		}
		input := AdditionalPermissions{Filesystem: map[string]Access{}, Network: call.AdditionalPermissions.Network}
		for p, a := range call.AdditionalPermissions.Filesystem {
			if !filepath.IsAbs(p) && !strings.HasPrefix(p, "~") {
				p = filepath.Join(call.Dir, p)
			}
			input.Filesystem[p] = a
		}
		additional, err := normalizeAdditional(input)
		if err != nil {
			return nil, err
		}
		if !s.hasAdditionalCache(additional) {
			if policy == ApprovalNever {
				return nil, errors.New("当前审批策略 never 拒绝额外权限申请")
			}
			result, err := s.approve(ctx, ApprovalRequest{
				Kind:          ApprovalAdditionalPermissions,
				Tool:          call.Tool,
				Command:       call.Command,
				Dir:           call.Dir,
				Justification: call.Justification,
				Additional:    &additional,
			})
			if err != nil || result.Decision == Deny {
				return nil, deniedError(result, err)
			}
			if result.Decision == AllowSession {
				s.mu.Lock()
				s.cache.additional = append(s.cache.additional, additional)
				s.mu.Unlock()
			}
		}
		return mergeAdditional(base, additional), nil

	case RequireEscalated:
		if policy == ApprovalNever {
			return nil, errors.New("当前审批策略 never 拒绝完全提权申请")
		}
		if strings.TrimSpace(call.Justification) == "" {
			return nil, errors.New("require_escalated 必须提供 justification")
		}
		key := filepath.Clean(call.Dir) + "\x00" + call.Command
		if call.Command == "" {
			key += "\x00" + call.Operation + "\x00" + call.Path
		}
		if !s.hasEscalationCache(key) {
			if policy == ApprovalNever {
				return nil, errors.New("当前审批策略 never 拒绝完全提权申请")
			}
			result, err := s.approve(ctx, ApprovalRequest{
				Kind:          ApprovalEscalation,
				Tool:          call.Tool,
				Command:       call.Command,
				Dir:           call.Dir,
				Justification: call.Justification,
			})
			if err != nil || result.Decision == Deny {
				return nil, deniedError(result, err)
			}
			if result.Decision == AllowSession {
				s.addEscalationCache(key)
			}
		}
		full := cloneProfile(base)
		full.FullAccess = true
		full.Name = ":danger-full-access (approved)"
		return full, nil

	default:
		return nil, fmt.Errorf("未知 sandbox_permissions: %s", request)
	}
}

func requiresUntrustedApproval(call CallPermissions) bool {
	if call.Tool == "bash" || call.Tool == "write_file" || call.Tool == "edit_file" {
		return true
	}
	return call.Operation == "write" || call.Operation == "edit"
}

func approvalKindForCall(call CallPermissions) ApprovalKind {
	if call.Tool == "bash" || call.Operation == "command" {
		return ApprovalCommand
	}
	return ApprovalFileWrite
}

func untrustedCacheKey(call CallPermissions) string {
	dir := filepath.Clean(call.Dir)
	if call.Tool == "bash" || call.Operation == "command" {
		return "command\x00" + dir + "\x00" + call.Command
	}
	path := call.Path
	if normalized, err := NormalizePath(path); err == nil {
		path = normalized
	}
	return "write\x00" + dir + "\x00" + call.Operation + "\x00" + path
}

func normalizeAdditional(input AdditionalPermissions) (AdditionalPermissions, error) {
	if len(input.Filesystem) == 0 && input.Network == NetworkDisabled {
		return AdditionalPermissions{}, errors.New("额外权限不能为空")
	}
	if input.Network < NetworkDisabled || input.Network > NetworkFull {
		return AdditionalPermissions{}, errors.New("网络权限非法")
	}
	result := AdditionalPermissions{Filesystem: make(map[string]Access), Network: input.Network}
	for rawPath, access := range input.Filesystem {
		if strings.ContainsAny(rawPath, "*?{}[]") {
			return AdditionalPermissions{}, errors.New("额外权限必须使用字面路径")
		}
		if access != AccessRead && access != AccessWrite {
			return AdditionalPermissions{}, fmt.Errorf("额外权限不能使用 deny: %s", rawPath)
		}
		path, err := NormalizePath(rawPath)
		if err != nil {
			return AdditionalPermissions{}, err
		}
		if old, ok := result.Filesystem[path]; !ok || access > old {
			result.Filesystem[path] = access
		}
	}
	return result, nil
}

func mergeAdditional(base *Profile, additional AdditionalPermissions) *Profile {
	result := cloneProfile(base)
	for path, access := range additional.Filesystem {
		result.Files = append(result.Files, FileRule{Path: path, Access: access, Approved: true})
	}
	if additional.Network > result.Network {
		result.Network = additional.Network
	}
	return result
}

func cloneProfile(profile *Profile) *Profile {
	if profile == nil {
		return nil
	}
	result := *profile
	result.Files = append([]FileRule(nil), profile.Files...)
	result.ProtectedRoots = append([]string(nil), profile.ProtectedRoots...)
	if profile.Domains != nil {
		result.Domains = make(map[string]DomainRule, len(profile.Domains))
		for key, value := range profile.Domains {
			result.Domains[key] = value
		}
	}
	return &result
}

func deniedError(result ApprovalResult, err error) error {
	if err != nil {
		return fmt.Errorf("审批失败: %w", err)
	}
	message := "用户拒绝了权限申请"
	if strings.TrimSpace(result.Feedback) != "" {
		message += ": " + strings.TrimSpace(result.Feedback)
	}
	return errors.New(message)
}

func (s *Session) approve(ctx context.Context, request ApprovalRequest) (ApprovalResult, error) {
	if err := ctx.Err(); err != nil {
		return ApprovalResult{Decision: Deny}, err
	}
	result, err := s.approver.Approve(ctx, request)
	if ctx.Err() != nil {
		return ApprovalResult{Decision: Deny}, ctx.Err()
	}
	if result.Decision != AllowOnce && result.Decision != AllowSession {
		result.Decision = Deny
	}
	return result, err
}

func (s *Session) hasUntrustedCache(key string, call CallPermissions) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ok bool
	if call.Tool == "bash" || call.Operation == "command" {
		_, ok = s.cache.commands[key]
	} else {
		_, ok = s.cache.writeOps[key]
	}
	return ok
}

func (s *Session) addCommandCache(key string, call CallPermissions) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if call.Tool == "bash" || call.Operation == "command" {
		s.cache.commands[key] = struct{}{}
	} else {
		s.cache.writeOps[key] = struct{}{}
	}
}

func (s *Session) hasEscalationCache(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.cache.escalations[key]
	return ok
}

func (s *Session) addEscalationCache(key string) {
	s.mu.Lock()
	s.cache.escalations[key] = struct{}{}
	s.mu.Unlock()
}

func (s *Session) hasAdditionalCache(request AdditionalPermissions) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cached := range s.cache.additional {
		if additionalSubset(request, cached) {
			return true
		}
	}
	return false
}

func additionalSubset(request, cached AdditionalPermissions) bool {
	for path, access := range request.Filesystem {
		covered := false
		for parent, cachedAccess := range cached.Filesystem {
			if isStrictChildOrEqual(parent, path) && cachedAccess >= access {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	if request.Network > cached.Network {
		return false
	}
	return true
}
