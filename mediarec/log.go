package mediarec

import "log"

// logf 统一日志出口（与项目其它部分一致用标准 log）。
func logf(format string, args ...any) { log.Printf("mediarec: "+format, args...) }
