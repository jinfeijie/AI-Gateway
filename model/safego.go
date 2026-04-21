package model

import "log"

// SafeGo 启动受保护的 goroutine，panic 时记录日志而不是崩溃
func SafeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC] goroutine %s: %v", name, r)
			}
		}()
		fn()
	}()
}
