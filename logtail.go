package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
)

const webUILogReadBudget int64 = 1 << 20 // 仅管理日志接口，不限制模型请求或上游响应。

// readTailLines 按块倒序定位，再一次性拼接；复杂度与扫描字节数线性相关。
// 预算截断时丢弃首个不完整行，避免显示无法可靠脱敏的秘密值片段。
func readTailLines(f *os.File, n int, budget int64) ([]string, bool, error) {
	if n <= 0 {
		return []string{}, false, nil
	}
	st, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !st.Mode().IsRegular() {
		return nil, false, fmt.Errorf("只允许读取普通日志文件")
	}
	const chunkSize int64 = 16 << 10
	pos, scanned := st.Size(), int64(0)
	var chunks [][]byte
	newlines := 0
	for pos > 0 && newlines <= n && (budget <= 0 || scanned < budget) {
		size := min(chunkSize, pos)
		if budget > 0 {
			size = min(size, budget-scanned)
		}
		pos -= size
		chunk := make([]byte, int(size))
		count, err := f.ReadAt(chunk, pos)
		if err != nil && err != io.EOF {
			return nil, false, err
		}
		chunk = chunk[:count]
		chunks = append(chunks, chunk)
		scanned += int64(count)
		newlines += bytes.Count(chunk, []byte{'\n'})
		if count != int(size) {
			return nil, false, fmt.Errorf("日志在读取期间发生截断，请重试")
		}
	}
	truncated := pos > 0 && budget > 0 && scanned >= budget && newlines <= n
	data := make([]byte, 0, int(scanned))
	for i := len(chunks) - 1; i >= 0; i-- {
		data = append(data, chunks[i]...)
	}
	if pos > 0 {
		if end := bytes.IndexByte(data, '\n'); end >= 0 {
			data = data[end+1:]
		} else {
			data = nil
		}
	}
	lines := make([]string, 0, n)
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) != 0 {
			lines = append(lines, string(line))
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, truncated, nil
}
