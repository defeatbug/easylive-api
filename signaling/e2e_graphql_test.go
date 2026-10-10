// GraphQL 接口的端到端测试（无需数据库）。
//
// 能验证什么：接口确实接到了 gqlgen（而不再是 501 占位）、schema 正确加载、
// 数据库不可用时给出的是**可读的业务错误**而不是 panic 或空数据。
// 真正的数据读写验证见 store/store_integration_test.go（需要 DATABASE_URL）。
package signaling_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

type gqlRequest struct {
	Query string `json:"query"`
}

type gqlResponse struct {
	Data struct {
		Rooms *struct {
			ID string `json:"id"`
		} `json:"rooms"`
		Schema *struct {
			QueryType struct {
				Name string `json:"name"`
			} `json:"queryType"`
		} `json:"__schema"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func postGraphQL(t *testing.T, base, query string) (int, gqlResponse) {
	t.Helper()
	raw, _ := json.Marshal(gqlRequest{Query: query})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(base+"/graphql", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("请求 /graphql 失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out gqlResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestE2EGraphQL(t *testing.T) {
	ts := startServerFull(t, 0, "", "", "test")
	base := "http://" + strings.TrimSuffix(strings.TrimPrefix(ts.url, "ws://"), "/ws")

	// ---- 1) schema 能加载（内省查询走通说明不再是占位 handler）----
	code, out := postGraphQL(t, base, `{ __schema { queryType { name } } }`)
	if code != http.StatusOK {
		t.Fatalf("/graphql 应返回 200，实际 %d", code)
	}
	if out.Data.Schema == nil || out.Data.Schema.QueryType.Name != "Query" {
		t.Fatalf("内省查询应返回 Query 类型，实际 %+v（错误：%+v）", out.Data.Schema, out.Errors)
	}

	// ---- 2) 没有数据库时：给出可读的业务错误，而不是 panic/空数据 ----
	// 测试服务器传入的 store 是 nil，正好覆盖"数据库是可选依赖"这条路径。
	_, out = postGraphQL(t, base, `{ rooms { id title } }`)
	if len(out.Errors) == 0 {
		t.Fatalf("没有数据库时应返回错误，实际 data=%+v", out.Data)
	}
	if !strings.Contains(out.Errors[0].Message, "数据库不可用") {
		t.Fatalf("错误信息应说明数据库不可用，实际 %q", out.Errors[0].Message)
	}

	// ---- 3) 未定义字段：由 gqlgen 自己拒绝（说明 schema 生效）----
	_, out = postGraphQL(t, base, `{ notARealField }`)
	if len(out.Errors) == 0 {
		t.Fatalf("未知字段应被 schema 拒绝")
	}
}
