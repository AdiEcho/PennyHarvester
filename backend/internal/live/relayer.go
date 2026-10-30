package live

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"pennyharvester/backend/internal/engine"
)

// defaultRelayerURL is the public Polymarket relayer. It is a separate service
// from the CLOB: relaying only broadcasts already-signed wallet batches, so a
// Relayer API key can never place an order.
const defaultRelayerURL = "https://relayer-v2.polymarket.com"

// relayerKeysPath lists the keys owned by the address that authenticates the
// request. It is the cheapest authenticated endpoint, which makes it a safe
// credential check: it submits nothing and returns no wallet state.
const relayerKeysPath = "/relayer/api/keys"

// relayerKey is one entry of GET /relayer/api/keys.
type relayerKey struct {
	APIKey  string `json:"apiKey"`
	Address string `json:"address"`
}

// ProbeRelayer verifies a Relayer API key against the relayer itself.
//
// The docs are explicit that POST /submit still requires a signed batch
// (`from`, `proxyWallet`, `nonce` and `signature` are all required), and that
// RELAYER_API_KEY_ADDRESS must match the address that owns the key. So the
// check here is: the key authenticates, and its owner is the address derived
// from the configured private key.
func (e *Executor) ProbeRelayer(ctx context.Context, settings engine.LiveSettings, clobBaseURL, apiKey, address string) (engine.RelayerProbe, error) {
	if e == nil {
		return engine.RelayerProbe{}, fmt.Errorf("未配置实盘执行器")
	}
	apiKey = strings.TrimSpace(apiKey)
	address = normalizeKeyPrefix(strings.TrimSpace(address))
	if apiKey == "" || address == "" {
		return engine.RelayerProbe{}, fmt.Errorf("Relayer API Key 与签名者地址必须同时填写")
	}
	if !common.IsHexAddress(address) {
		return engine.RelayerProbe{}, fmt.Errorf("Relayer 签名者地址不是有效的 0x 地址")
	}
	e.mu.Lock()
	effective, err := e.resolveLocked(settings, clobBaseURL)
	e.mu.Unlock()
	if err != nil {
		return engine.RelayerProbe{}, err
	}
	privateKey, err := crypto.HexToECDSA(strings.TrimPrefix(effective.PrivateKey, "0x"))
	if err != nil {
		return engine.RelayerProbe{}, fmt.Errorf("无法从私钥推导签名地址: %w", err)
	}
	signer := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()
	if !strings.EqualFold(signer, address) {
		return engine.RelayerProbe{}, fmt.Errorf("签名者地址 %s 与私钥地址 %s 不一致；Relayer 密钥必须由该私钥对应的地址创建", address, signer)
	}

	base := strings.TrimRight(strings.TrimSpace(effective.RelayerURL), "/")
	if base == "" {
		base = defaultRelayerURL
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+relayerKeysPath, nil)
	if err != nil {
		return engine.RelayerProbe{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("RELAYER_API_KEY", apiKey)
	request.Header.Set("RELAYER_API_KEY_ADDRESS", address)

	start := time.Now()
	client := http.DefaultClient
	if effective.Proxy != nil {
		client = effective.Proxy.HTTPClient(15 * time.Second)
	}
	response, err := client.Do(request)
	if err != nil {
		return engine.RelayerProbe{}, fmt.Errorf("连接 Polymarket Relayer: %w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	latency := float64(time.Since(start).Microseconds()) / 1000
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return engine.RelayerProbe{}, fmt.Errorf("Relayer 拒绝了这把密钥（401）：API Key 或签名者地址不正确，或密钥已被吊销")
	case http.StatusForbidden:
		return engine.RelayerProbe{}, fmt.Errorf("Relayer 不允许该鉴权方式（403）")
	case http.StatusTooManyRequests:
		return engine.RelayerProbe{}, fmt.Errorf("Relayer 请求配额已用尽（429），请稍后重试")
	default:
		return engine.RelayerProbe{}, fmt.Errorf("Relayer 返回 %s: %s", response.Status, truncateBody(body))
	}
	var keys []relayerKey
	if err := json.Unmarshal(body, &keys); err != nil {
		return engine.RelayerProbe{}, fmt.Errorf("解析 Relayer 密钥列表失败: %w", err)
	}
	if len(keys) == 0 {
		return engine.RelayerProbe{}, fmt.Errorf("该签名者地址下没有任何 Relayer 密钥，这把 Key 可能已被吊销")
	}
	found := false
	for _, key := range keys {
		if strings.EqualFold(strings.TrimSpace(key.APIKey), apiKey) {
			found = true
			break
		}
	}
	if !found {
		return engine.RelayerProbe{}, fmt.Errorf("该地址的 %d 把 Relayer 密钥中不包含这把 Key；请确认它是在 %s 下创建的", len(keys), address)
	}
	return engine.RelayerProbe{
		Signer:    signer,
		Address:   address,
		APIKey:    maskAPIKey(apiKey),
		KeyCount:  len(keys),
		LatencyMS: latency,
	}, nil
}

func truncateBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		return text[:200] + "…"
	}
	return text
}
