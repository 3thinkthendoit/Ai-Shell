package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-shell/internal/vault"
)

// 「测试模型配置」的价值几乎全在错误信息上。
//
// 原始错误形如 `LLM 返回 404: {"error":{"message":"not found"}}` ——
// 用户看不出问题出在地址、密钥还是模型名。这些用例锁住的是
// 「每个常见状态码都要给出可照做的建议」，而不只是把状态码复述一遍。
func TestDiagnoseLLMError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		err    string
		hasKey bool
		want   []string // 必须都出现
	}{
		{
			name: "404 要明确指出 Base URL 填多了端点",
			// 这是最常见的配置错误，也是最难自己看出来的一个
			status: 404,
			err:    `LLM 返回 404: {"error":{"message":"not found"}}`,
			hasKey: true,
			want:   []string{"不要带 /chat/completions", "https://token.sensenova.cn/v1/chat/completions"},
		},
		{
			name:   "401 且配了密钥 → 指向密钥本身",
			status: 401,
			err:    "LLM 返回 401: unauthorized",
			hasKey: true,
			want:   []string{"认证失败", "API Key 不正确"},
		},
		{
			// 首次运行的默认状态：Base URL 预置成 OpenAI，用户还没填 Key。
			// 这时说「API Key 不正确」会让他以为自己填错了 —— 而他根本没填。
			name:   "401 但压根没配密钥 → 说的是「没配」而不是「不正确」",
			status: 401,
			err:    "LLM 返回 401: unauthorized",
			hasKey: false,
			want:   []string{"认证失败", "还没有配置 API Key"},
		},
		{
			name:   "403 且没配密钥 → 同样提示没配",
			status: 403,
			err:    "LLM 返回 403: forbidden",
			hasKey: false,
			want:   []string{"还没有配置 API Key"},
		},
		{
			name:   "403 且配了密钥 → 指向权限",
			status: 403,
			err:    "LLM 返回 403: forbidden",
			hasKey: true,
			want:   []string{"认证失败", "权限"},
		},
		{
			name:   "429 指向限流",
			status: 429,
			err:    "LLM 返回 429: rate limited",
			hasKey: true,
			want:   []string{"限流", "额度"},
		},
		{
			name:   "400 多半是模型名写错",
			status: 400,
			err:    "LLM 返回 400: model not found",
			hasKey: true,
			want:   []string{"模型名", "model not found"},
		},
		{
			name:   "5xx 要说明不是用户的问题",
			status: 503,
			err:    "LLM 返回 503: unavailable",
			hasKey: true,
			want:   []string{"服务端", "不是你的配置"},
		},
		{
			name:   "连不上（status 0）与 HTTP 错误要区分开",
			status: 0,
			err:    "dial tcp 127.0.0.1:1: connect: connection refused",
			hasKey: true,
			want:   []string{"连不上", "connection refused"},
		},
		{
			name:   "超时单独给一句人话",
			status: 0,
			err:    "Post \"http://x\": context deadline exceeded",
			hasKey: true,
			want:   []string{"超时", "代理"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := diagnoseLLMError(errors.New(c.err), c.status, "https://token.sensenova.cn/v1", c.hasKey)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("提示里应包含 %q，实得: %s", w, got)
				}
			}
		})
	}
}

// 兜底：未知状态码也不能把原始错误吞掉 —— 否则用户连线索都没有。
func TestDiagnoseLLMErrorFallsBackToRawMessage(t *testing.T) {
	got := diagnoseLLMError(errors.New("某种没见过的错误"), 418, "https://x/v1", true)
	if !strings.Contains(got, "某种没见过的错误") {
		t.Fatalf("未知状态码应回落到原始错误，实得: %s", got)
	}
}

// ListLLMModels 的 404/400 要给 /models 专属提示：这两个状态码走通用诊断会误导 ——
// 404 会被说成「地址填多了端点」，400 会被说成「模型名写错了」（此时模型名还没填）。
func TestListLLMModelsSpecialStatusHints(t *testing.T) {
	t.Setenv("AISHELL_KEYFILE", "1")
	v := vault.New(t.TempDir())
	if err := v.Open(); err != nil {
		t.Fatalf("打开凭证库失败: %v", err)
	}
	a := &App{v: v}

	cases := []struct {
		name     string
		status   int
		want     string // 专属提示的标志词
		notWants []string
	}{
		{
			name:   "404 → 未实现 /models 接口，而不是地址填错了",
			status: http.StatusNotFound,
			want:   "没有实现标准 /models 接口",
			notWants: []string{
				"chat/completions", // 通用诊断会让用户去删 URL 里的端点 —— 但这里 URL 是对的
			},
		},
		{
			name:   "400 → 网关拒绝，而不是模型名写错了",
			status: http.StatusBadRequest,
			want:   "网关拒绝了这次 /models 请求",
			notWants: []string{
				"写错", // 通用诊断会归因到模型名 —— 但调 /models 时模型名还没填
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
			}))
			defer srv.Close()

			res := a.ListLLMModels("", srv.URL, "")
			if res.OK {
				t.Fatalf("%d 不应成功", c.status)
			}
			if !strings.Contains(res.Message, c.want) {
				t.Errorf("提示应包含 %q，实得: %s", c.want, res.Message)
			}
			for _, nw := range c.notWants {
				if strings.Contains(res.Message, nw) {
					t.Errorf("提示不应包含 %q（对 /models 是误导），实得: %s", nw, res.Message)
				}
			}
		})
	}
}
