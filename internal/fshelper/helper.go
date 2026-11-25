package fshelper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/aka-xubo/my-tiny-claw/internal/fsops"
	"github.com/aka-xubo/my-tiny-claw/internal/sandbox"
)

type Request struct {
	Op            string `json:"op"`
	Path          string `json:"path"`
	Content       string `json:"content,omitempty"`
	OldText       string `json:"old_text,omitempty"`
	NewText       string `json:"new_text,omitempty"`
	MaxBytes      int    `json:"max_bytes,omitempty"`
	CreateParents bool   `json:"create_parents,omitempty"`
}

type Response struct {
	OK        bool   `json:"ok"`
	Content   string `json:"content,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	Errno     string `json:"errno,omitempty"`
	Error     string `json:"error,omitempty"`
}

func Serve(input io.Reader, output io.Writer) error {
	reader := bufio.NewReader(input)
	line, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	var request Request
	if err := json.Unmarshal(line, &request); err != nil {
		return writeResponse(output, Response{Error: fmt.Sprintf("请求解析失败: %v", err)})
	}
	response := execute(request)
	return writeResponse(output, response)
}

func Run(ctx context.Context, session *sandbox.Session, executable, workDir string, profile *sandbox.Profile, request Request) (Response, error) {
	if session == nil {
		return Response{}, errors.New("沙箱会话不能为空")
	}
	cmd, cleanup, err := session.Spawn(ctx, []string{executable, "__fs-helper"}, workDir, profile)
	if err != nil {
		return Response{}, err
	}
	defer cleanup()

	encoded, err := json.Marshal(request)
	if err != nil {
		return Response{}, err
	}
	cmd.Stdin = bytes.NewReader(append(encoded, '\n'))
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=en_US.UTF-8", "TMPDIR=" + os.TempDir()}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	responseBytes, err := cmd.Output()
	if err != nil {
		return Response{}, fmt.Errorf("helper 执行失败: %w: %s", err, stderr.String())
	}
	var response Response
	if err := json.Unmarshal(responseBytes, &response); err != nil {
		return Response{}, fmt.Errorf("helper 响应解析失败: %w", err)
	}
	return response, nil
}

func execute(request Request) Response {
	path := request.Path
	if !filepath.IsAbs(path) {
		path = filepath.Clean(path)
	}
	if resolved, err := resolveExistingTarget(path); err == nil {
		path = resolved
	}
	switch request.Op {
	case "read":
		_, response := read(path, request.MaxBytes)
		return response
	case "write":
		return write(path, request.Content, request.CreateParents)
	case "edit":
		content, response := read(path, 0)
		if !response.OK {
			return response
		}
		updated, err := fsops.FuzzyReplace(content, request.OldText, request.NewText)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return write(path, updated, false)
	default:
		return Response{Error: "不支持的文件操作: " + request.Op}
	}
}

func read(path string, maxBytes int) (string, Response) {

	file, err := os.Open(path)
	if err != nil {
		return "", errorResponse(err, fmt.Sprintf("读取文件失败: %v", err))
	}
	defer file.Close()
	var input io.Reader = file
	if maxBytes > 0 {
		input = io.LimitReader(file, int64(maxBytes)+1)
	}
	data, err := io.ReadAll(input)
	if err != nil {
		return "", errorResponse(err, fmt.Sprintf("读取文件失败: %v", err))
	}
	if maxBytes > 0 && len(data) > maxBytes {
		return string(data[:maxBytes]), Response{OK: true, Content: string(data[:maxBytes]), Truncated: true}
	}

	return string(data), Response{OK: true, Content: string(data)}
}

func write(path, content string, createParents bool) Response {
	if createParents {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return errorResponse(err, fmt.Sprintf("创建父目录失败: %v", err))
		}
	}
	mode := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claw-write-*")
	fallback := errors.Is(err, syscall.EPERM)
	if err == nil {
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		if _, err = tmp.WriteString(content); err == nil {
			err = tmp.Sync()
		}
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Chmod(tmpName, mode)
		}
		if err == nil {
			err = os.Rename(tmpName, path)
			fallback = errors.Is(err, syscall.EPERM)
		}
	}
	if err == nil {
		return Response{OK: true}
	}

	// A writable file may sit in a directory where creating a sibling or
	// renaming is forbidden. Match shell redirection by falling back to an
	// in-place write when the target already exists.

	if !fallback {
		return errorResponse(err, fmt.Sprintf("写入文件失败: %v", err))
	}
	file, openErr := os.OpenFile(path, os.O_WRONLY, mode)
	if openErr != nil {
		return errorResponse(err, fmt.Sprintf("写入文件失败: %v", err))
	}

	info, statErr := file.Stat()
	if statErr != nil || hasMultipleLinks(info) {
		file.Close()
		if statErr != nil {
			return errorResponse(statErr, statErr.Error())
		}
		return Response{Error: "拒绝原地写入多硬链接文件"}
	}
	if err := file.Truncate(0); err != nil {
		file.Close()
		return errorResponse(err, err.Error())
	}
	_, writeErr := file.WriteString(content)
	if syncErr := file.Sync(); writeErr == nil {
		writeErr = syncErr
	}
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return errorResponse(writeErr, fmt.Sprintf("写入文件失败: %v", writeErr))
	}
	return Response{OK: true}
}

func resolveExistingTarget(path string) (string, error) {
	if _, err := os.Lstat(path); err != nil {
		return path, err
	}
	return filepath.EvalSymlinks(path)
}

func errorResponse(err error, message string) Response {
	errno := ""
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		errno = pathErr.Err.Error()
	}
	if errors.Is(err, syscall.EPERM) {
		errno = "EPERM"
	}
	return Response{Errno: errno, Error: message}
}

func writeResponse(output io.Writer, response Response) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = output.Write(append(encoded, '\n'))
	return err
}
