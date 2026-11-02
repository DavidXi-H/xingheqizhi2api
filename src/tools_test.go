package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func shellTool(name string) toolSpec {
	var t toolSpec
	t.Type = "function"
	t.Function.Name = name
	t.Function.Description = "run a command"
	t.Function.Parameters = json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"}}}`)
	return t
}

func TestParseToolCallsStandard(t *testing.T) {
	text := `<tool_call>{"name":"exec_command","arguments":{"cmd":"ls -la"}}</tool_call>`
	rest, calls := parseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("期望 1 个调用，得到 %d", len(calls))
	}
	if calls[0].Function.Name != "exec_command" {
		t.Errorf("name = %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"cmd":"ls -la"}` {
		t.Errorf("args = %q", calls[0].Function.Arguments)
	}
	if rest != "" {
		t.Errorf("正文应被清空，得到 %q", rest)
	}
}

func TestParseToolCallsWithPreamble(t *testing.T) {
	text := "我先看一下目录。\n<tool_call>{\"name\":\"shell\",\"arguments\":{\"command\":\"pwd\"}}</tool_call>"
	rest, calls := parseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("期望 1 个调用，得到 %d", len(calls))
	}
	if !strings.Contains(rest, "先看一下") {
		t.Errorf("正文应保留前缀说明，得到 %q", rest)
	}
}

func TestParseToolCallsFenced(t *testing.T) {
	text := "```tool_call\n{\"name\":\"shell\",\"arguments\":{\"command\":\"ls\"}}\n```"
	_, calls := parseToolCalls(text)
	if len(calls) != 1 || calls[0].Function.Name != "shell" {
		t.Fatalf("应识别代码块内的调用，得到 %+v", calls)
	}
}

func TestParseToolCallsBareJSON(t *testing.T) {
	text := `{"name":"shell","arguments":"{\"command\":\"ls\"}"}`
	_, calls := parseToolCalls(text)
	if len(calls) != 1 {
		t.Fatalf("应识别裸 JSON，得到 %d", len(calls))
	}
	if calls[0].Function.Arguments != `{"command":"ls"}` {
		t.Errorf("字符串形式的 arguments 应被规范化，得到 %q", calls[0].Function.Arguments)
	}
}

func TestParseToolCallsNone(t *testing.T) {
	text := "递归就是把问题拆成同类子问题。"
	rest, calls := parseToolCalls(text)
	if calls != nil {
		t.Fatalf("普通回答不应识别出调用")
	}
	if rest != text {
		t.Errorf("正文不应被改动")
	}
}

func TestParseToolCallsParallel(t *testing.T) {
	text := `<tool_call>{"name":"a","arguments":{}}</tool_call><tool_call>{"name":"b","arguments":{}}</tool_call>`
	_, calls := parseToolCalls(text)
	if len(calls) != 2 {
		t.Fatalf("期望 2 个调用，得到 %d", len(calls))
	}
}

func TestMatchToolName(t *testing.T) {
	tools := []toolSpec{shellTool("exec_command"), shellTool("write_stdin"), shellTool("view_image")}
	cases := []struct{ in, want string }{
		{"exec_command", "exec_command"},
		{"EXEC_COMMAND", "exec_command"},
		{"functions.exec_command", "exec_command"},
		{"mcp__local__exec_command", "exec_command"},
		{"shell", "exec_command"},
		{"bash", "exec_command"},
	}
	for _, c := range cases {
		got, ok := matchToolName(c.in, tools)
		if !ok || got != c.want {
			t.Errorf("matchToolName(%q) = %q, %v；期望 %q", c.in, got, ok, c.want)
		}
	}
	if _, ok := matchToolName("totally_unrelated", tools); ok {
		t.Errorf("无关名字不应被匹配")
	}
}

func TestMatchToolNameSingleTool(t *testing.T) {
	tools := []toolSpec{shellTool("apply_patch")}
	if got, ok := matchToolName("whatever", tools); !ok || got != "apply_patch" {
		t.Errorf("只有一个工具时应直接归到它，得到 %q %v", got, ok)
	}
}

func TestBuildPromptWithTools(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: "你是助手"},
		{Role: "user", Content: "看看目录"},
		{Role: "assistant", Content: "", ToolCalls: []toolCall{{
			ID: "c1", Type: "function",
			Function: toolFunction{Name: "shell", Arguments: `{"command":"pwd"}`},
		}}},
		{Role: "tool", ToolCallID: "c1", Name: "shell", Content: "/tmp/demo"},
	}
	p := buildPrompt(msgs, []toolSpec{shellTool("shell")})
	for _, want := range []string{
		"运行模式覆盖", "工具调用协议", "你是助手", "[对话历史]", "用户: 看看目录",
		`<tool_call>{"name":"shell","arguments":{"command":"pwd"}}</tool_call>`,
		"[工具结果 shell]", "/tmp/demo",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt 缺少片段 %q", want)
		}
	}
}

func TestBuildPromptWithoutTools(t *testing.T) {
	p := buildPrompt([]chatMessage{{Role: "user", Content: "你好"}}, nil)
	if strings.Contains(p, "工具调用协议") {
		t.Errorf("没有工具时不应注入协议")
	}
	if !strings.Contains(p, "你好") {
		t.Errorf("prompt 丢了正文")
	}
}

func TestLooksLikeUnfoundedRefusal(t *testing.T) {
	yes := []string{
		"这份 PPT 我还没有读到，而 /Users/x/1102-大模型越狱.pptx 不在我可读的文件系统里。",
		"请把 PPT 拖进当前对话/工作区，或把作业要求截图发来。",
		"我无法读取该文件，可能是权限问题。",
		"没有权限读取这个目录。",
		"实在访问不到这个路径，麻烦你重新上传。",
	}
	for _, s := range yes {
		if !looksLikeUnfoundedRefusal(s) {
			t.Errorf("应判为无据放弃: %q", s)
		}
	}
	no := []string{
		"已经修好 price.py，3 个测试全部通过。",
		"当前目录下有 3 个 .txt 文件，已写入 report.md。",
		"这个项目没有 config.yaml，我创建了 no-config.txt 说明情况。",
		"结论：越狱攻击的常见手段有三类。",
		"第 3 页和第 6 页是纯图片，读不到文字标题，我按图表页标注了。",
		"第 20 页原文已整理完，notes.md 写在当前目录。",
	}
	for _, s := range no {
		if looksLikeUnfoundedRefusal(s) {
			t.Errorf("不该判为无据放弃: %q", s)
		}
	}
}

func TestLastToolLooksFailed(t *testing.T) {
	ok := []chatMessage{
		{Role: "user", Content: "读一下 pptx"},
		{Role: "tool", Content: "Process exited with code 0\nOutput:\n-rw-r--r--@ 1 u staff 5050109 1102-大模型越狱.pptx"},
	}
	if lastToolLooksFailed(ok) {
		t.Error("退出码 0 不该判为失败")
	}
	bad := []chatMessage{
		{Role: "tool", Content: "Process exited with code 1\nOutput:\nNo such file or directory"},
	}
	if !lastToolLooksFailed(bad) {
		t.Error("No such file 应判为失败")
	}
	none := []chatMessage{{Role: "user", Content: "hi"}}
	if lastToolLooksFailed(none) {
		t.Error("没有工具结果时不该判为失败")
	}
}
