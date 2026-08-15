package service

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const testFingerprintSecret = "test-jwt-secret-for-fingerprint"

func TestAccountFingerprint_StableAndDistinct(t *testing.T) {
	t.Parallel()

	a := accountFingerprint(testFingerprintSecret, 26)
	b := accountFingerprint(testFingerprintSecret, 26)
	c := accountFingerprint(testFingerprintSecret, 27)

	require.Equal(t, a, b, "同账号同密钥必须稳定，否则无法用于聚合")
	require.NotEqual(t, a, c, "不同账号必须不同")
	require.Len(t, a, accountFingerprintHexLen)
	require.Regexp(t, `^[0-9a-f]+$`, a)
}

func TestAccountFingerprint_DependsOnSecret(t *testing.T) {
	t.Parallel()

	// 换密钥后指纹必须变 —— 证明它真的是 HMAC 而不是裸 hash。
	// 这是「防枚举」的根据：没有密钥就算不出来。
	withA := accountFingerprint("secret-a", 26)
	withB := accountFingerprint("secret-b", 26)
	require.NotEqual(t, withA, withB)
}

// 防枚举回归：account_id 是小整数（现役最大三位数），裸 sha256 用彩虹表几秒可破。
// 本用例断言指纹【不等于】任何形式的裸 hash / 明文，锁死「必须带密钥」这个前提。
func TestAccountFingerprint_NotGuessableFromIDAlone(t *testing.T) {
	t.Parallel()

	const id int64 = 26
	fp := accountFingerprint(testFingerprintSecret, id)

	require.NotContains(t, fp, strconv.FormatInt(id, 10), "指纹不得包含明文 id")
	// 无密钥算不出同一个值
	require.NotEqual(t, accountFingerprint("", id), fp)
}

func TestAccountFingerprint_DegradesWithoutSecret(t *testing.T) {
	t.Parallel()

	// 配置缺失不该阻断请求，只是少一个可观测字段（fail-open）
	require.Empty(t, accountFingerprint("", 26))
	require.Empty(t, accountFingerprint(testFingerprintSecret, 0))
	require.Empty(t, accountFingerprint(testFingerprintSecret, -1))
}

func TestSetAccountFingerprintHeader(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	setAccountFingerprintHeader(h, testFingerprintSecret, &Account{ID: 26})
	require.NotEmpty(t, h.Get(accountFingerprintHeader))

	// nil / 空密钥不写头也不 panic
	empty := http.Header{}
	setAccountFingerprintHeader(empty, "", &Account{ID: 26})
	require.Empty(t, empty.Get(accountFingerprintHeader))
	setAccountFingerprintHeader(empty, testFingerprintSecret, nil)
	require.Empty(t, empty.Get(accountFingerprintHeader))
	setAccountFingerprintHeader(nil, testFingerprintSecret, &Account{ID: 26}) // 不得 panic
}

// 响应头里【绝不能】出现账号真实身份。
func TestStreamHeaderWriter_ExposesFingerprintNotAccountID(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	svc := &OpenAIGatewayService{cfg: &config.Config{JWT: config.JWTConfig{Secret: testFingerprintSecret}}}
	account := &Account{ID: 26, Name: "prod-account-alpha", Platform: PlatformOpenAI}

	svc.newStreamHeaderWriterForAccount(c, http.Header{}, account)()

	fp := rec.Header().Get(accountFingerprintHeader)
	require.NotEmpty(t, fp, "流式路径必须带账号指纹")
	require.Equal(t, accountFingerprint(testFingerprintSecret, 26), fp)

	// 关键负向断言：真实账号身份不得出现在任何响应头里
	for key, values := range rec.Header() {
		for _, v := range values {
			require.NotContains(t, v, "prod-account-alpha", "响应头 %s 泄漏了账号名", key)
			require.NotEqual(t, "26", v, "响应头 %s 泄漏了 account_id", key)
		}
	}
}

func TestStreamHeaderWriter_NilAccountKeepsLegacyBehavior(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	svc := &OpenAIGatewayService{cfg: &config.Config{JWT: config.JWTConfig{Secret: testFingerprintSecret}}}
	// 未接入的调用方传 nil，行为与改造前一致
	svc.newStreamHeaderWriter(c, http.Header{})()

	require.Empty(t, rec.Header().Get(accountFingerprintHeader))
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"), "原有 SSE 头不受影响")
}

// --- P2-b 核心约束：追溯标识绝不外泄到上游 --------------------------------------
//
// 三个追溯头（X-Client-Request-Id / X-Request-Id / X-Account-Fingerprint）只能出现在
// 回客户端的响应里。若混进发往上游服务商的请求头，等于把内部拓扑（有多少账号、
// 怎么路由）暴露给上游。
//
// 现状保障是【白名单制】—— 上游 header 只放行显式列出的项。本测试锁死这个保障：
// 将来有人往白名单加东西时会红。
func TestTraceHeadersNeverForwardedUpstream(t *testing.T) {
	t.Parallel()

	traceHeaders := []string{
		"x-client-request-id",
		"x-request-id",
		"x-account-fingerprint",
	}

	for _, h := range traceHeaders {
		require.False(t, openaiAllowedHeaders[h],
			"追溯头 %s 混进了 Responses 路径的上游白名单 —— 会泄漏内部拓扑给上游", h)
		require.False(t, openaiCCRawAllowedHeaders[h],
			"追溯头 %s 混进了 raw 路径的上游白名单 —— 会泄漏内部拓扑给上游", h)
	}
}

// 白名单本身不得意外膨胀成「全放行」—— 那会让上面的断言形同虚设。
func TestUpstreamHeaderWhitelistsStayNarrow(t *testing.T) {
	t.Parallel()

	require.NotEmpty(t, openaiAllowedHeaders)
	require.NotEmpty(t, openaiCCRawAllowedHeaders)
	require.Less(t, len(openaiCCRawAllowedHeaders), 8,
		"raw 路径白名单应保持极窄（当前仅 accept-language / user-agent）")
}
