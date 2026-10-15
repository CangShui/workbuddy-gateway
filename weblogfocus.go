package main

import (
	"io"
	"os"
	"strconv"
	"strings"
)

// parseLogOffset 解析告警带来的日志定位偏移；缺省或非法时返回 -1 表示按尾部读取。
func parseLogOffset(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return -1
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return -1
	}
	return v
}

// readWebLogAround 读取覆盖指定字节偏移的日志窗口，供「告警 → 跳到那一行」使用。
// 返回窗口内的行、目标行下标、以及是否真的定位到了（文件被轮转或清理时为 false）。
func readWebLogAround(name string, offset int64, span int64) ([]string, int, bool, error) {
	root, err := os.OpenRoot(logDir)
	if err != nil {
		return nil, -1, false, err
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		return nil, -1, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, -1, false, err
	}
	size := info.Size()
	resolved := offset <= size
	start, end := offset-span, offset+span
	if start < 0 {
		start = 0
	}
	if end > size {
		end = size
	}
	if end-start > webUILogReadBudget {
		end = start + webUILogReadBudget
	}
	if end <= start {
		return nil, -1, resolved, nil
	}
	buf := make([]byte, end-start)
	n, readErr := f.ReadAt(buf, start)
	if readErr != nil && readErr != io.EOF {
		return nil, -1, false, readErr
	}
	buf = buf[:n]
	// 记录窗口首行在文件中的真实起点，便于把偏移换算成行下标。
	cursor := start
	if start > 0 {
		if cut := strings.IndexByte(string(buf), '\n'); cut >= 0 {
			cursor += int64(cut) + 1
			buf = buf[cut+1:]
		} else {
			buf = nil
		}
	}
	lines := strings.Split(string(buf), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	focus := -1
	for i, line := range lines {
		next := cursor + int64(len(line)) + 1
		if offset >= cursor && offset < next {
			focus = i
			break
		}
		cursor = next
	}
	return lines, focus, resolved && focus >= 0, nil
}
