package config

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"aura/pkg/log"

	"github.com/fsnotify/fsnotify"
)

// reloadDebounceDelay 防抖延迟：编辑器 truncate+write 或原子 rename 保存时，fsnotify
// 可能短时间内触发多次（甚至先读到空文件）。延迟一小段再读，确保写完、避免热更到中间态。
const reloadDebounceDelay = 200 * time.Millisecond

// relevantOps 仅这些事件视为「配置内容可能已变更」。
const relevantOps = fsnotify.Write | fsnotify.Create | fsnotify.Rename

var (
	watcher     *fsnotify.Watcher
	reloadTimer *time.Timer
	reloadMu    sync.Mutex
	// watchClosed 标记 stopWatch 已被调用：定时器即便已经被 time.AfterFunc 派发进自身
	// goroutine，回调入口也能据此短路，避免在关停后改写全局配置指针。
	watchClosed atomic.Bool
)

// startWatch 监听配置文件所在目录，目标文件发生写入/创建/重命名时（防抖后）触发热更。
//
// 监听目录而非文件本身：很多编辑器用「写临时文件 + 原子 rename 覆盖」保存，直接监听
// 文件会在 rename 后丢失 inode，导致后续修改收不到事件。
func startWatch(path string) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("config: 创建文件监听器失败: %w", err)
	}

	dir := filepath.Dir(path)
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return fmt.Errorf("config: 监听目录 %q 失败: %w", dir, err)
	}

	watcher = w
	watchClosed.Store(false)
	go watchLoop(w, filepath.Clean(path))
	return nil
}

// watchLoop 持续消费监听事件，仅对目标文件的相关事件触发防抖热更。
func watchLoop(w *fsnotify.Watcher, target string) {
	for {
		select {
		case event, ok := <-w.Events:
			if !ok {
				return
			}
			if filepath.Clean(event.Name) == target && event.Op&relevantOps != 0 {
				scheduleReload()
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Errorf("[Config] 文件监听错误: %v", err)
		}
	}
}

// scheduleReload 以防抖方式触发热更：重置定时器，延迟 reloadDebounceDelay 后执行 doReload。
// 通过 watchClosed 守卫，使关停后立即触发的事件不会再走到 doReload。
func scheduleReload() {
	reloadMu.Lock()
	defer reloadMu.Unlock()
	if watchClosed.Load() {
		return
	}
	if reloadTimer != nil {
		reloadTimer.Stop()
	}
	reloadTimer = time.AfterFunc(reloadDebounceDelay, func() {
		// 定时器可能已被派发到自身 goroutine，而 stopWatch 已经执行；
		// 这里再次检查，确保关停后绝不修改全局配置。
		if watchClosed.Load() {
			return
		}
		doReload()
	})
}

// stopWatch 停止定时器并关闭监听器。可安全多次调用。
func stopWatch() {
	watchClosed.Store(true)

	reloadMu.Lock()
	if reloadTimer != nil {
		reloadTimer.Stop()
		reloadTimer = nil
	}
	reloadMu.Unlock()

	if watcher != nil {
		_ = watcher.Close()
		watcher = nil
	}
}
