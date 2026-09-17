package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// -----------------------------------------------------------------------------
// reset 命令
//
// 清空除登录凭据（workbuddy*.json）以外的全部本地数据：
//   - 状态快照 workbuddy-status.json
//   - 模型缓存 wb-models-cache.json（含倍率与价格探测结论）
//   - 失效标记 *.disabled / *.json.disabled
//   - 运行日志 logs/
//
// 然后重新拉取模型目录与倍率并写入新缓存。
// 内存中的账号账本由 serve 进程持有，reset 无法直接修改；serve 运行时请重启服务
// 使内存状态与磁盘一起归零。
// -----------------------------------------------------------------------------

func runReset() {
	fmt.Println("================ WorkBuddy 本地数据清理 ================")

	authPaths := collectConfiguredAuthPaths()
	removed := 0

	removeIfExists := func(path, label string) {
		if err := os.Remove(path); err == nil {
			fmt.Printf("  已删除%s: %s\n", label, path)
			removed++
		} else if !os.IsNotExist(err) {
			fmt.Printf("  删除失败: %s: %v\n", path, err)
		}
	}

	// 1. 状态快照与模型缓存（含临时文件）
	for _, f := range []string{
		statusSnapshotFile, statusSnapshotFile + ".tmp",
		modelsCacheFile, modelsCacheFile + ".tmp",
	} {
		removeIfExists(f, "")
	}

	// 2. 失效标记：显式凭据路径对应的标记文件
	for _, p := range authPaths {
		removeIfExists(markerPath(p), "失效标记")
	}
	// 自动发现模式下可能残留的其他标记文件
	if entries, err := os.ReadDir("."); err == nil {
		for _, e := range entries {
			name := e.Name()
			if strings.HasSuffix(name, ".disabled") || strings.HasSuffix(name, ".json.disabled") {
				removeIfExists(name, "失效标记")
			}
		}
	}

	// 3. 运行日志
	if entries, err := os.ReadDir(logDir); err == nil {
		n := 0
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if err := os.Remove(filepath.Join(logDir, e.Name())); err == nil {
				n++
			}
		}
		if err := os.RemoveAll(logDir); err == nil {
			_ = os.MkdirAll(logDir, 0755)
		}
		fmt.Printf("  已清空日志: %s/（%d 个文件）\n", logDir, n)
		removed += n
	}

	fmt.Printf("清理完成，共删除 %d 项；登录凭据已保留（%d 个）\n", removed, len(authPaths))

	// 4. 重新拉取模型目录与倍率
	fmt.Println("\n正在重新拉取模型目录与倍率...")
	loadModelsCache()
	if err := loadAccounts(); err != nil {
		fmt.Printf("未找到可用凭据，跳过模型拉取: %v\n", err)
	} else {
		refreshModelsOnce()
	}
	ids, source := mergedModelIDs()
	fmt.Printf("模型目录来源: %s，模型数: %d\n", modelSourceLabel(source), len(ids))
	fmt.Println("提示：若 serve 正在运行，请重启服务使内存状态同步归零：")
	fmt.Println("  systemctl restart workbuddy-gateway")
	fmt.Println("=======================================================")
}
