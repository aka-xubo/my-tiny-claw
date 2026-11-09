package context

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const MaxSkillBodyBytes = 64 * 1024

// SkillDescriptor 是启动阶段暴露给模型的轻量技能元数据。
type SkillDescriptor struct {
	Name         string
	Description  string
	RelativePath string
}

// Skill 是按需读取后的完整技能。
type Skill struct {
	SkillDescriptor
	Body string
}

type cachedSkill struct {
	body    string
	modTime time.Time
	size    int64
}

// SkillLoader 负责建立技能元数据索引，并按技能名称读取正文。
// 索引阶段只读取 frontmatter，不读取 Markdown 正文。
type SkillLoader struct {
	workDir string

	mu        sync.RWMutex
	index     map[string]SkillDescriptor
	bodyCache map[string]cachedSkill
}

func NewSkillLoader(workDir string) *SkillLoader {
	return &SkillLoader{
		workDir:   workDir,
		index:     make(map[string]SkillDescriptor),
		bodyCache: make(map[string]cachedSkill),
	}
}

// ListDescriptors 刷新并返回按名称排序的技能目录。
func (s *SkillLoader) ListDescriptors() []SkillDescriptor {
	if err := s.refreshIndex(); err != nil {
		log.Printf("[Skills] 技能索引刷新失败: %v\n", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	descriptors := make([]SkillDescriptor, 0, len(s.index))
	for _, descriptor := range s.index {
		descriptors = append(descriptors, descriptor)
	}
	sort.Slice(descriptors, func(i, j int) bool {
		return descriptors[i].Name < descriptors[j].Name
	})
	return descriptors
}

// LoadBody 根据技能名称按需读取完整正文。名称不接受路径，避免目录穿越。
func (s *SkillLoader) LoadBody(name string) (Skill, error) {
	name = strings.TrimSpace(name)
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) || name == "." || name == ".." {
		return Skill{}, fmt.Errorf("技能名称无效: %q", name)
	}

	if err := s.refreshIndex(); err != nil {
		return Skill{}, err
	}

	s.mu.RLock()
	descriptor, ok := s.index[name]
	cached, cachedOK := s.bodyCache[name]
	s.mu.RUnlock()
	if !ok {
		return Skill{}, fmt.Errorf("未找到技能 %q", name)
	}

	fullPath, err := s.safeSkillPath(descriptor.RelativePath)
	if err != nil {
		return Skill{}, err
	}
	resolvedPath, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		return Skill{}, fmt.Errorf("解析技能文件真实路径失败: %w", err)
	}
	baseDir, err := filepath.Abs(filepath.Join(s.workDir, ".claw", "skills"))
	if err != nil {
		return Skill{}, fmt.Errorf("解析技能目录失败: %w", err)
	}
	resolvedBaseDir, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		return Skill{}, fmt.Errorf("解析技能目录真实路径失败: %w", err)
	}
	if resolvedPath != resolvedBaseDir && !strings.HasPrefix(resolvedPath, resolvedBaseDir+string(os.PathSeparator)) {
		return Skill{}, fmt.Errorf("技能文件真实路径超出技能目录范围")
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		return Skill{}, fmt.Errorf("读取技能文件失败: %w", err)
	}
	if info.Size() > MaxSkillBodyBytes {
		return Skill{}, fmt.Errorf("技能 %q 正文超过 %d 字节限制", name, MaxSkillBodyBytes)
	}
	if cachedOK && cached.modTime.Equal(info.ModTime()) && cached.size == info.Size() {
		return Skill{SkillDescriptor: descriptor, Body: cached.body}, nil
	}

	file, err := os.Open(fullPath)
	if err != nil {
		return Skill{}, fmt.Errorf("读取技能文件失败: %w", err)
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, MaxSkillBodyBytes+1))
	if err != nil {
		return Skill{}, fmt.Errorf("读取技能正文失败: %w", err)
	}
	if len(content) > MaxSkillBodyBytes {
		return Skill{}, fmt.Errorf("技能 %q 正文超过 %d 字节限制", name, MaxSkillBodyBytes)
	}

	skill, err := parseSkillDocument(string(content), descriptor)
	if err != nil {
		return Skill{}, fmt.Errorf("解析技能 %q 失败: %w", name, err)
	}

	s.mu.Lock()
	s.bodyCache[name] = cachedSkill{body: skill.Body, modTime: info.ModTime(), size: info.Size()}
	s.mu.Unlock()
	return skill, nil
}

func (s *SkillLoader) refreshIndex() error {
	baseDir := filepath.Join(s.workDir, ".claw", "skills")
	_, err := os.Stat(baseDir)
	if os.IsNotExist(err) {
		s.mu.Lock()
		s.index = make(map[string]SkillDescriptor)
		s.bodyCache = make(map[string]cachedSkill)
		s.mu.Unlock()
		return nil
	}
	if err != nil {
		return fmt.Errorf("扫描技能目录失败: %w", err)
	}

	newIndex := make(map[string]SkillDescriptor)
	err = filepath.WalkDir(baseDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "SKILL.md" {
			return nil
		}

		descriptor, parseErr := parseSkillMetadata(path, baseDir)
		if parseErr != nil {
			log.Printf("[Skills] 跳过无效技能 %s: %v\n", path, parseErr)
			return nil
		}
		if _, exists := newIndex[descriptor.Name]; exists {
			log.Printf("[Skills] 跳过重复技能名称: %s\n", descriptor.Name)
			return nil
		}
		newIndex[descriptor.Name] = descriptor
		return nil
	})
	if err != nil {
		return fmt.Errorf("建立技能索引失败: %w", err)
	}

	s.mu.Lock()
	s.index = newIndex
	for name := range s.bodyCache {
		if _, exists := newIndex[name]; !exists {
			delete(s.bodyCache, name)
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *SkillLoader) safeSkillPath(relativePath string) (string, error) {
	baseDir, err := filepath.Abs(filepath.Join(s.workDir, ".claw", "skills"))
	if err != nil {
		return "", fmt.Errorf("解析技能目录失败: %w", err)
	}
	fullPath, err := filepath.Abs(filepath.Join(baseDir, relativePath))
	if err != nil {
		return "", fmt.Errorf("解析技能路径失败: %w", err)
	}
	if fullPath != baseDir && !strings.HasPrefix(fullPath, baseDir+string(os.PathSeparator)) {
		return "", fmt.Errorf("技能路径超出技能目录范围")
	}
	return fullPath, nil
}

func parseSkillMetadata(path string, baseDir string) (SkillDescriptor, error) {
	file, err := os.Open(path)
	if err != nil {
		return SkillDescriptor{}, err
	}
	defer file.Close()

	name, description, err := readFrontmatter(bufio.NewScanner(file))
	if err != nil {
		return SkillDescriptor{}, err
	}
	relativePath, err := filepath.Rel(baseDir, path)
	if err != nil {
		return SkillDescriptor{}, err
	}
	return SkillDescriptor{Name: name, Description: description, RelativePath: relativePath}, nil
}

func readFrontmatter(scanner *bufio.Scanner) (string, string, error) {
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", "", err
		}
		return "", "", fmt.Errorf("文件为空")
	}
	firstLine := strings.TrimPrefix(scanner.Text(), "\ufeff")
	if strings.TrimSpace(firstLine) != "---" {
		return "", "", fmt.Errorf("缺少 YAML frontmatter 起始标记")
	}

	var name, description string
	closed := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "---" {
			closed = true
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			name = strings.TrimSpace(value)
		case "description":
			description = strings.TrimSpace(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", err
	}
	if !closed {
		return "", "", fmt.Errorf("缺少 YAML frontmatter 结束标记")
	}
	if name == "" {
		return "", "", fmt.Errorf("frontmatter 缺少 name")
	}
	if description == "" {
		return "", "", fmt.Errorf("frontmatter 缺少 description")
	}
	return name, description, nil
}

func parseSkillDocument(content string, descriptor SkillDescriptor) (Skill, error) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) < 2 || strings.TrimSpace(strings.TrimPrefix(lines[0], "\ufeff")) != "---" {
		return Skill{}, fmt.Errorf("缺少有效 YAML frontmatter")
	}
	closingLine := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closingLine = i
			break
		}
	}
	if closingLine < 0 {
		return Skill{}, fmt.Errorf("缺少 YAML frontmatter 结束标记")
	}
	return Skill{SkillDescriptor: descriptor, Body: strings.TrimSpace(strings.Join(lines[closingLine+1:], "\n"))}, nil
}
