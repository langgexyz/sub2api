package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
)

// 账号指纹（IK8VUZ）—— 让客户端能按「上游账号」维度聚合问题，但拿不到账号真实身份。
//
// 场景：客户端观察到某类异常（如工具参数退化，见 IK8VPN）后，需要回答「是不是某个上游
// 账号在持续出问题」。直接回传 account_id 会把内部拓扑（有多少账号、怎么路由）暴露给
// 客户端乃至更外层；完全不回传则客户端只能看到一堆孤立现象，无法聚合。
//
// 折中：回传不可逆的指纹。客户端能聚合「fingerprint a3f2c8d1 的退化率 40%」，
// 但推不出它是哪个账号；网关侧凭 fingerprint 反查一步到位。
//
// --- 为什么必须 HMAC 而不是裸 hash -------------------------------------------
//
// account_id 是小整数（现役最大三位数）。裸 sha256(26) 的取值空间只有几百个，
// 彩虹表几秒即可反推 —— 那等于没脱敏。加服务端密钥后，客户端没有密钥就只能把它
// 当不透明标签用。
//
// --- 密钥来源与轮换 -----------------------------------------------------------
//
// 复用 JWT secret 派生，不新增配置项。密钥轮换后 fingerprint 随之变化，
// 历史数据不再对得上 —— 这是预期行为（指纹只用于近期问题聚合，不是长期标识）。

const (
	// accountFingerprintHeader 是回给【客户端】的响应头。
	//
	// 安全约束（IK8VUZ）：本头与 X-Client-Request-Id / X-Request-Id 一样，
	// 只能出现在回客户端的响应里，【绝不能】混进发往上游服务商的请求头 ——
	// 否则等于把内部账号拓扑暴露给上游。
	//
	// 现状保障：两条转发路径的上游 header 都是【白名单制】
	// （openaiAllowedHeaders / openaiCCRawAllowedHeaders），本头不在白名单里，
	// 因此默认进不去。这是隐式保证，故用负向单测锁死 —— 将来有人往白名单加东西时，
	// 测试会红。见 openai_account_fingerprint_test.go。
	accountFingerprintHeader = "X-Account-Fingerprint"

	// 指纹长度（hex 字符数）。12 位 = 48 bit，碰撞概率对「几百个账号」的规模远够用，
	// 又不至于长到干扰日志阅读。
	accountFingerprintHexLen = 12

	// HKDF 风格的域分隔：同一密钥派生不同用途时互不影响。
	accountFingerprintContext = "sub2api:account-fingerprint:v1"
)

// accountFingerprint 计算账号指纹。
//
// secret 为空时返回空串（降级为不输出该头）—— 配置缺失不该阻断请求，
// 只是少一个可观测字段。
func accountFingerprint(secret string, accountID int64) string {
	if secret == "" || accountID <= 0 {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte(accountFingerprintContext)); err != nil {
		return ""
	}
	if _, err := mac.Write([]byte{0}); err != nil { // 域分隔符，避免 context 与 id 拼接产生歧义
		return ""
	}
	if _, err := mac.Write([]byte(strconv.FormatInt(accountID, 10))); err != nil {
		return ""
	}
	return hex.EncodeToString(mac.Sum(nil))[:accountFingerprintHexLen]
}

// setAccountFingerprintHeader 在响应头上写入账号指纹。
//
// 调用时机：必须在任何响应体写出【之前】—— HTTP 头一旦随首个 chunk 发出就改不了了。
// 流式路径尤其要注意：SSE 的第一帧一发，header 就锁定。
//
// 直接收 http.Header（而非包一层接口）：它本身就是可直接构造的 map 类型，
// 单测里 http.Header{} 即可，不需要 mock。
func setAccountFingerprintHeader(h http.Header, secret string, account *Account) {
	if h == nil || account == nil {
		return
	}
	if fp := accountFingerprint(secret, account.ID); fp != "" {
		h.Set(accountFingerprintHeader, fp)
	}
}
