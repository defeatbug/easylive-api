package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// deepseekURL 是 DeepSeek 对话接口（OpenAI 兼容格式）。
const deepseekURL = "https://api.deepseek.com/chat/completions"

// summarySystemPrompt 是纪要生成的系统提示词：约束输出为中文 Markdown，只写有依据的内容。
const summarySystemPrompt = `你是专业的会议纪要助手。根据下面的会议转写文本（格式为 [时间] 说话人：内容），生成一份中文 Markdown 会议纪要。

输出结构（只包含有实质内容的章节，没有就不写）：
# 会议纪要
## 核心要点
- 每条一句话，记录讨论的关键信息
## 待办事项
- [ ] 事项描述（负责人，如有提及）
## 决定事项
- 会上明确拍板的内容

要求：
- 简洁，不写废话
- 只写转写文本中有明确依据的内容，绝不编造、绝不脑补人名和数字
- 如果转写内容太少或无实质内容，纪要可以很短，但不要输出"无内容"之类的占位话
- 直接输出 Markdown 正文，不要加任何解释性前后缀`

// Complete 调 DeepSeek 生成纪要，返回 markdown 文本。
func Complete(ctx context.Context, apiKey, model, user string) (string, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": summarySystemPrompt},
			{"role": "user", "content": user},
		},
		"temperature": 0.3,
		"max_tokens":  2000,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deepseekURL, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("deepseek: 构造请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 150 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("deepseek: 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("deepseek: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("deepseek: 解析响应失败: %w", err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("deepseek: API 错误: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("deepseek: 响应无 choices")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}
