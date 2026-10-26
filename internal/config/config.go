// Package config 承载 ZCode2api 的运行配置。
// 语义对齐 Python 版 server.py 的 load_config()：默认值 + config.json 覆盖 + 环境变量覆盖。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config 全量配置；字段语义见各注释。
type Config struct {
	Port          int      `json:"port"`
	Upstream      string   `json:"upstream"`
	TokenSource   string   `json:"-"` // config.json 的 token_source，显式最高优先
	ClientVersion string   `json:"client_version"`
	DefaultModel  string   `json:"default_model"`
	Models        []string `json:"models"`
	ModelAlias    map[string]string
	APIKey        string // 本服务自身的入口 key，空 = 不鉴权
	RequestLog    string
	DumpDir       string // 上游请求/响应落盘目录，空 = 关闭
	DumpMaxFiles  int
}

// Default 与 Python 版 load_config 的内置默认一致。
func Default() *Config {
	return &Config{
		Port:          3010, // 对齐终版 Python server.py（FastAPI:3010）
		Upstream:      "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages",
		ClientVersion: "3.16.3",
		DefaultModel:  "GLM-5.3-Flash",
		Models:        []string{"GLM-5.3-Flash", "GLM-5.3"},
		ModelAlias: map[string]string{
			"glm-5.3-flash": "GLM-5.3-Flash",
			"glm-5.3":       "GLM-5.3",
			"glm-4.7":       "GLM-4.7",
		},
		RequestLog:   "request.log",
		DumpDir:      "dumps",
		DumpMaxFiles: 200,
	}
}

// setdefault：环境变量优先，否则保留默认（对齐 V02api 的 LoadEnv 语义）。
func setdefault(dst *string, env, def string) {
	if v := os.Getenv(env); v != "" {
		*dst = v
	} else {
		*dst = def
	}
}

// Load 装配顺序：默认值 → 同目录/工作目录 config.json → 环境变量。
func Load() (*Config, error) {
	cfg := Default()

	// config.json 与 Python 版同放仓库根（Python 目录只读，Go 版自带一份）
	for _, p := range []string{"config.json", filepath.Join("..", "ZCode2api", "config.json")} {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var j struct {
			Port          int      `json:"port"`
			Upstream      string   `json:"upstream"`
			TokenSource   string   `json:"token_source"`
			ClientVersion string   `json:"client_version"`
			DefaultModel  string   `json:"default_model"`
			Models        []string `json:"models"`
		}
		if err := json.Unmarshal(raw, &j); err != nil {
			return nil, fmt.Errorf("config.json 解析失败: %w", err)
		}
		if j.Port != 0 {
			cfg.Port = j.Port
		}
		if j.Upstream != "" {
			cfg.Upstream = j.Upstream
		}
		if j.TokenSource != "" {
			cfg.TokenSource = j.TokenSource
		}
		if j.ClientVersion != "" {
			cfg.ClientVersion = j.ClientVersion
		}
		if j.DefaultModel != "" {
			cfg.DefaultModel = j.DefaultModel
		}
		if len(j.Models) != 0 {
			cfg.Models = j.Models
		}
		break
	}

	setdefault(&cfg.APIKey, "API_KEY", "")
	setdefault(&cfg.TokenSource, "ZCODE_TOKEN", cfg.TokenSource)
	setdefault(&cfg.DumpDir, "DUMP_DIR", cfg.DumpDir)
	if v := os.Getenv("DUMP"); v == "0" {
		cfg.DumpDir = ""
	}
	if v := os.Getenv("DUMP_MAX_FILES"); v != "" {
		n := 0
		for _, c := range v {
			if c < '0' || c > '9' {
				n = -1
				break
			}
			n = n*10 + int(c-'0')
		}
		if n > 0 {
			cfg.DumpMaxFiles = n
		}
	}
	if v := os.Getenv("PORT"); v != "" {
		n := 0
		for _, c := range v {
			if c < '0' || c > '9' {
				n = -1
				break
			}
			n = n*10 + int(c-'0')
		}
		if n > 0 {
			cfg.Port = n
		}
	}
	// 相对 request.log 落到可执行目录旁，行为对齐 Python 版 HERE/request.log
	if cfg.RequestLog != "" && !filepath.IsAbs(cfg.RequestLog) {
		if exe, err := os.Executable(); err == nil {
			cfg.RequestLog = filepath.Join(filepath.Dir(exe), cfg.RequestLog)
		}
	}
	return cfg, nil
}

// NormModel 模型别名归一（Python norm_model 语义：原名/小名都查 alias 表）。
func (c *Config) NormModel(name string) string {
	if name == "" {
		return c.DefaultModel
	}
	if m, ok := c.ModelAlias[name]; ok {
		return m
	}
	if m, ok := c.ModelAlias[strings.ToLower(name)]; ok {
		return m
	}
	return name
}
