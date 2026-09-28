package main

import (
	"errors"
	"strings"
	"testing"
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
