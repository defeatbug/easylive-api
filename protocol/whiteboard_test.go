package protocol

import (
	"math"
	"testing"
)

// TestWhiteboardSanitize 覆盖服务端对客户端提交的裁剪与拒绝。
//
// 为什么值得单测：白板的输入直接来自客户端，越界坐标会让别人的画布出现画面外的笔迹，
// 超大点数组会拖垮所有人的渲染，而 By/Seq 必须由服务端填写——这些都是"不信客户端"的边界。
func TestWhiteboardSanitize(t *testing.T) {
	// 合法的一笔：坐标越界应被夹到 [0,1]，颜色非法应回退而不是丢弃
	op := WhiteboardOp{Kind: WhiteboardStroke, Stroke: &Stroke{
		Tool: "pen", Color: "not-a-color", Width: 0,
		Points: [][2]float64{{-0.5, 0.5}, {1.5, 2}},
	}}
	if err := op.Sanitize(); err != nil {
		t.Fatalf("合法笔画不应报错: %v", err)
	}
	if op.Stroke.Color != "#ff4d4f" {
		t.Fatalf("非法颜色应回退为默认色，实际 %s", op.Stroke.Color)
	}
	if op.Stroke.Width != 3 {
		t.Fatalf("宽度 0 应回退为 3，实际 %v", op.Stroke.Width)
	}
	if op.Stroke.Points[0][0] != 0 || op.Stroke.Points[1][0] != 1 || op.Stroke.Points[1][1] != 1 {
		t.Fatalf("坐标应被夹到 [0,1]，实际 %v", op.Stroke.Points)
	}

	// 非法输入必须被拒绝
	cases := []struct {
		name string
		op   WhiteboardOp
	}{
		{"未知操作", WhiteboardOp{Kind: "hack"}},
		{"缺少 stroke", WhiteboardOp{Kind: WhiteboardStroke}},
		{"未知工具", WhiteboardOp{Kind: WhiteboardStroke, Stroke: &Stroke{Tool: "spray", Points: [][2]float64{{0, 0}}}}},
		{"没有点", WhiteboardOp{Kind: WhiteboardStroke, Stroke: &Stroke{Tool: "pen"}}},
		{"点数超限", WhiteboardOp{Kind: WhiteboardStroke, Stroke: &Stroke{Tool: "pen", Points: make([][2]float64, MaxWhiteboardPoints+1)}}},
		{"NaN 坐标", WhiteboardOp{Kind: WhiteboardStroke, Stroke: &Stroke{Tool: "pen", Points: [][2]float64{{math.NaN(), 0}}}}},
	}
	for _, c := range cases {
		if err := c.op.Sanitize(); err == nil {
			t.Fatalf("%s：应被拒绝", c.name)
		}
	}

	// clear/undo 不应携带笔画（顺手清掉，避免把大数组一路广播出去）
	cl := WhiteboardOp{Kind: WhiteboardClear, Stroke: &Stroke{Tool: "pen", Points: [][2]float64{{0, 0}}}}
	if err := cl.Sanitize(); err != nil || cl.Stroke != nil {
		t.Fatalf("clear 应被接受且清空 stroke，实际 err=%v stroke=%v", err, cl.Stroke)
	}
}

// TestWhiteboardOpValidation 覆盖颜色校验的边界（长度、前缀、非十六进制字符）。
func TestWhiteboardOpValidation(t *testing.T) {
	for _, bad := range []string{"", "#fff", "#FFFFFFF", "FFFFFF", "##FFFFFF", "#GGGGGG"} {
		if validHexColor(bad) {
			t.Fatalf("颜色 %q 应判为非法", bad)
		}
	}
	for _, ok := range []string{"#000000", "#ffffff", "#0aF9bC"} {
		if !validHexColor(ok) {
			t.Fatalf("颜色 %q 应判为合法", ok)
		}
	}
}
