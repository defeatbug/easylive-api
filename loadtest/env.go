package loadtest

import "os"

// envTokenAPIKey 读取 TOKEN_API_KEY：生产环境（APP_ENV!=development）的 /token
// 必须带 X-API-Key，压测脚本与评测流程都从这里取，避免把密钥写进命令行历史。
func envTokenAPIKey() string { return os.Getenv("TOKEN_API_KEY") }
