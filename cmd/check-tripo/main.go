// check-tripo 使用与服务一致的配置，只读验证接入；不初始化生产运行时。
package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/app"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/tripo"
)

func main() {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": false, "error": "读取 .env 失败"})
		os.Exit(1)
	}
	cfg, err := app.ConfigFromEnv()
	if err != nil {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": false, "error": "配置无效，请检查环境变量"})
		os.Exit(1)
	}
	c, err := tripo.NewWithBaseURL(cfg.TripoKey, cfg.TripoBaseURL)
	if err != nil {
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	os.Exit(check(ctx, os.Stdout, c))
}

func check(ctx context.Context, out io.Writer, c *tripo.Client) int {
	d, err := c.Preflight(ctx)
	result := map[string]any{"ok": err == nil, "base_url": c.BaseURL, "key_present": c.APIKey != ""}
	if err != nil {
		result["diagnostic"] = tripo.Describe(err, "preflight").Safe(c.APIKey)
	} else {
		result["http_status"], result["provider_code"] = d.HTTPStatus, d.ProviderCode
		result["provider_trace_id"], result["duration_ms"] = d.ProviderTraceID, d.DurationMS
	}
	if e := json.NewEncoder(out).Encode(result); e != nil || err != nil {
		return 1
	}
	return 0
}
