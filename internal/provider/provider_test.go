package provider

import (
	"testing"

	"cybercompanion/internal/config"
)

func TestResolve_DefaultEndpointRouting(t *testing.T) {
	// 模拟未配置 providers，只有默认 oneapi_url 与 model
	cfg := &config.Config{
		OneAPIURL:   "http://127.0.0.1:3000/v1/chat/completions",
		OneAPIToken: "sk-test",
		Model:       "default-chat",
		Routing: config.ModelRouting{
			Complex: "deepseek-reasoner",
			Extract: "sensenova-6.8-flash-lite",
		},
	}
	config.SetForTest(cfg)

	r := NewRegistry(nil)

	// 1. 未显式路由的用途（chat）应回退到全局默认模型
	epChat, err := r.Resolve(PurposeChat)
	if err != nil {
		t.Fatalf("Resolve(PurposeChat) failed: %v", err)
	}
	if epChat.Model != "default-chat" {
		t.Errorf("expected model 'default-chat', got %q", epChat.Model)
	}

	// 2. 显式路由的用途（complex）应解析为 deepseek-reasoner
	epComplex, err := r.Resolve(PurposeComplex)
	if err != nil {
		t.Fatalf("Resolve(PurposeComplex) failed: %v", err)
	}
	if epComplex.Model != "deepseek-reasoner" {
		t.Errorf("expected model 'deepseek-reasoner', got %q", epComplex.Model)
	}
	if epComplex.URL != "http://127.0.0.1:3000/v1/chat/completions" {
		t.Errorf("expected URL http://127.0.0.1:3000/v1/chat/completions, got %q", epComplex.URL)
	}

	// 3. 显式路由的用途（extract）应解析为 sensenova-6.8-flash-lite
	epExtract, err := r.Resolve(PurposeExtract)
	if err != nil {
		t.Fatalf("Resolve(PurposeExtract) failed: %v", err)
	}
	if epExtract.Model != "sensenova-6.8-flash-lite" {
		t.Errorf("expected model 'sensenova-6.8-flash-lite', got %q", epExtract.Model)
	}
}
