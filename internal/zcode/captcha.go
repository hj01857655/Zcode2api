package zcode

// 阿里云无痕验证码求解：起 node 子进程跑 tools/captcha_solve.js，
// 由 happy-dom + 阿里云 SDK 完成 PoW/签名，输出 captchaVerifyParam（一次性）。
//
// 依赖（随仓库分发在 tools/）：
//
//	tools/captcha_solve.js      求解脚本（happy-dom 需 node_modules）
//	tools/AliyunCaptcha.js      阿里云 SDK 样本（公开 CDN 文件副本）
//
// 可用环境变量覆盖：ZCODE_NODE（node 路径）、ZCODE_CAPTCHA_SCRIPT、ZCODE_CAPTCHA_SDK。
import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SolveCaptcha 产出阿里云 captchaVerifyParam（一次性）。scene/prefix/region
// 来自 client/configs 动态下发（FetchCaptchaConfig），空值时脚本用内置默认。
// 偶发失败（网络时序 / PoW 竞态）重试一次，返回最后一次的错误。
func SolveCaptcha(scene, prefix, region string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		param, err := solveOnce(scene, prefix, region)
		if err == nil {
			return param, nil
		}
		lastErr = err
	}
	return "", lastErr
}

func solveOnce(scene, prefix, region string) (string, error) {
	script := os.Getenv("ZCODE_CAPTCHA_SCRIPT")
	if script == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		script = filepath.Join(filepath.Dir(exe), "tools", "captcha_solve.js")
	}
	if _, err := os.Stat(script); err != nil {
		return "", fmt.Errorf("求解脚本不存在: %s（把 tools/ 随网关部署）", script)
	}
	node := os.Getenv("ZCODE_NODE")
	if node == "" {
		node = "node"
	}
	cmd := exec.Command(node, script)
	cmd.Dir = filepath.Dir(script)
	env := os.Environ()
	if scene != "" {
		env = append(env, "SCENE_ID="+scene)
	}
	if prefix != "" {
		env = append(env, "CAPTCHA_PREFIX="+prefix)
	}
	if region != "" {
		env = append(env, "CAPTCHA_REGION="+region)
	}
	if sdk := os.Getenv("ZCODE_CAPTCHA_SDK"); sdk != "" {
		env = append(env, "CAPTCHA_SDK="+sdk)
	} else if _, err := os.Stat(filepath.Join(cmd.Dir, "AliyunCaptcha.js")); err == nil {
		env = append(env, "CAPTCHA_SDK="+filepath.Join(cmd.Dir, "AliyunCaptcha.js"))
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("node 求解失败: %w", err)
	}
	var res struct {
		OK    bool   `json:"ok"`
		Param string `json:"param"`
		Msg   string `json:"msg"`
	}
	line := strings.TrimSpace(string(out))
	if i := strings.LastIndex(line, "\n"); i >= 0 {
		line = line[i+1:]
	}
	if json.Unmarshal([]byte(line), &res) != nil {
		return "", fmt.Errorf("求解输出无法解析: %.120s", line)
	}
	if !res.OK || res.Param == "" {
		return "", fmt.Errorf("求解失败: %s", res.Msg)
	}
	return res.Param, nil
}
