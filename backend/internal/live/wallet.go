package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"pennyharvester/backend/internal/engine"
)

// Wallet discovery and wallet-type detection.
//
// Polymarket accounts hold their collateral in one of four wallet kinds:
//
//	0 EOA            the signer itself
//	1 Proxy          legacy Magic/Google EIP-1167 clone
//	2 Safe           legacy 1-of-1 Gnosis Safe
//	3 Deposit Wallet ERC-1271 wallet (default for accounts created after 2026-05)
//
// The CLOB's GET /auth/get-proxy-wallets is retired: the public deployment
// answers 404 and the path is gone from the CLOB spec, so discovery relies on
// two independent sources instead:
//
//  1. Polymarket's public profile service (Gamma /public-profile), which maps an
//     owner EOA to the account wallet. No key, signature or RPC needed.
//  2. Deterministic CREATE2 derivation of the legacy Proxy/Safe wallets plus a
//     deposit-wallet candidate, each verified with eth_getCode.
//
// Once the account wallet is known, its type is read from the derivation that
// produced it and otherwise from the deployed bytecode: an EIP-1167 clone is a
// legacy Proxy, an ERC-1967 / beacon clone (or an EIP-7702 delegation) is a
// Deposit Wallet, and a getOwners() answer identifies a Gnosis Safe.
const (
	safeFactoryAddress    = "0xaacFeEa03eb1561C4e67d661e40682Bd20E3541b"
	proxyFactoryAddress   = "0xaB45c5A4B0c941a2F231C04C3f49182e1A254052"
	depositFactoryAddress = "0x00000000000Fb5C9ADea0298D729A0CB3823Cc07"
	// depositImplementation is the UUPS implementation the deposit-wallet factory
	// clones for every account created before the beacon migration. It is the
	// only scheme whose CREATE2 address can be reproduced offline.
	depositImplementation = "0x58CA52ebe0DadfdF531Cde7062e76746de4Db1eB"

	safeInitCodeHash  = "0x2bce2127ff07fb632d16c8347c4ebf501f4841168bed00d9e6ef715ddb6fcecf"
	proxyInitCodeHash = "0xd21df8dc65880a8606f09fe0ce3df9b8869287ab0b058be05aa9e8af6330a00b"

	profilePath = "/public-profile"

	// defaultPolygonRPC is a keyless public Polygon endpoint. It is only used
	// for read-only calls (eth_getCode / eth_call) during wallet detection.
	defaultPolygonRPC = "https://polygon-bor-rpc.publicnode.com"
)

// Wallet type labels reported to the UI.
const (
	WalletTypeEOA     = 0
	WalletTypeProxy   = 1
	WalletTypeSafe    = 2
	WalletTypeDeposit = 3
)

func walletTypeName(signatureType int) string {
	switch signatureType {
	case WalletTypeEOA:
		return "EOA（Signer = Maker）"
	case WalletTypeProxy:
		return "Proxy（旧版 Magic / Google）"
	case WalletTypeSafe:
		return "Safe（旧版外部签名钱包）"
	case WalletTypeDeposit:
		return "Deposit Wallet（POLY_1271）"
	default:
		return "未知"
	}
}

var (
	eip1167Prefix = common.Hex2Bytes("363d3d373d3d3d363d73")
	eip7702Prefix = common.Hex2Bytes("ef0100")
	// beaconMarker / uupsMarker appear in the runtime bytecode of the two
	// ERC-1967 proxy flavours the deposit-wallet factory deploys.
	beaconMarker = common.Hex2Bytes("5c60da1b60e01b36527fa3f0ad74e5423aebfd80d3ef4346578335a9a72aeaee59ff6c")
	uupsMarker   = common.Hex2Bytes("360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc")
)

// create2Address mirrors keccak256(0xff ++ deployer ++ salt ++ initCodeHash)[12:].
func create2Address(factory common.Address, salt []byte, initCodeHash string) common.Address {
	return crypto.CreateAddress2(factory, common.BytesToHash(salt), common.FromHex(initCodeHash))
}

// DeriveSafeAddress returns the Gnosis Safe Polymarket would deploy for signer.
//
// Salt scheme: keccak256(abi.encode(signer)) — the 20-byte address is
// left-padded to 32 bytes first.
func DeriveSafeAddress(signer common.Address) common.Address {
	salt := crypto.Keccak256(common.LeftPadBytes(signer.Bytes(), 32))
	return create2Address(common.HexToAddress(safeFactoryAddress), salt, safeInitCodeHash)
}

// DeriveProxyAddress returns the legacy Magic/Google Proxy for signer.
//
// Salt scheme: keccak256(abi.encodePacked(signer)) — packed, no padding.
func DeriveProxyAddress(signer common.Address) common.Address {
	salt := crypto.Keccak256(signer.Bytes())
	return create2Address(common.HexToAddress(proxyFactoryAddress), salt, proxyInitCodeHash)
}

// DeriveDepositAddress returns the UUPS deposit wallet for signer.
//
// It mirrors the walletId/args/salt/bytecodeHash chain of the official
// builder-relayer client: walletId = bytes32(signer),
// args = abi.encode(factory, walletId), salt = keccak256(args) and the Solady
// ERC-1967 init code hash computed over the same args.
func DeriveDepositAddress(signer common.Address) common.Address {
	factory := common.HexToAddress(depositFactoryAddress)
	walletID := common.LeftPadBytes(signer.Bytes(), 32)
	args := append(common.LeftPadBytes(factory.Bytes(), 32), walletID...)
	salt := crypto.Keccak256(args)
	return create2Address(factory, salt, soladyERC1967InitCodeHash(depositImplementation, args))
}

// Solady LibClone.initCodeHashERC1967 constants. The pre-image is
// prefix(10) | implementation(20) | 0x6009 | const2 | const1 | args, where
// byte index 1 of the prefix is replaced by the immutable-args length.
var (
	erc1967Const1 = common.Hex2Bytes("cc3735a920a3ca505d382bbc545af43d6000803e6038573d6000fd5b3d6000f3")
	erc1967Const2 = common.Hex2Bytes("5155f3363d3d373d3d363d7f360894a13ba1a3210667c828492db98dca3e2076")
	erc1967Prefix = common.Hex2Bytes("61003d3d8160233d3973")
)

func soladyERC1967InitCodeHash(implementation string, args []byte) string {
	prefix := new(big.Int).SetBytes(erc1967Prefix)
	prefix.Add(prefix, new(big.Int).Lsh(big.NewInt(int64(len(args))), 56))
	pre := make([]byte, 0, 64+len(args))
	pre = append(pre, prefix.FillBytes(make([]byte, 10))...)
	pre = append(pre, common.HexToAddress(implementation).Bytes()...)
	pre = append(pre, 0x60, 0x09)
	pre = append(pre, erc1967Const2...)
	pre = append(pre, erc1967Const1...)
	pre = append(pre, args...)
	return crypto.Keccak256Hash(pre).Hex()
}

// walletSource names the origin of a discovered address so the UI can explain
// where a value came from.
type walletSource string

const (
	sourceProfile    walletSource = "polymarket-profile"
	sourceDerivation walletSource = "create2-derivation"
)

// walletCandidate is an address that belongs to the signer, in discovery order.
type walletCandidate struct {
	address  common.Address
	source   walletSource
	deployed bool
}

// rpcClient is a tiny JSON-RPC caller used for read-only Polygon calls. It
// shares the executor's outbound proxy so a restricted network still works.
type rpcClient struct {
	endpoint string
	http     *http.Client
}

// effectiveRPC returns the configured Polygon RPC endpoint or the keyless
// public default.
func effectiveRPC(value string) string {
	if trimmed := strings.TrimRight(strings.TrimSpace(value), "/"); trimmed != "" {
		return trimmed
	}
	return defaultPolygonRPC
}

func newRPCClient(cfg Config) *rpcClient {
	endpoint := effectiveRPC(cfg.PolygonRPC)
	client := &http.Client{Timeout: 12 * time.Second}
	if cfg.Proxy != nil {
		client = cfg.Proxy.HTTPClient(12 * time.Second)
	}
	return &rpcClient{endpoint: endpoint, http: client}
}

func (c *rpcClient) call(ctx context.Context, method string, params []any) (json.RawMessage, error) {
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("连接 Polygon RPC: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 Polygon RPC 响应: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Polygon RPC 返回 %s", response.Status)
	}
	var decoded struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("解析 Polygon RPC 响应: %w", err)
	}
	if decoded.Error != nil {
		return nil, fmt.Errorf("Polygon RPC 错误 %d: %s", decoded.Error.Code, decoded.Error.Message)
	}
	return decoded.Result, nil
}

func (c *rpcClient) code(ctx context.Context, address common.Address) ([]byte, error) {
	result, err := c.call(ctx, "eth_getCode", []any{address.Hex(), "latest"})
	if err != nil {
		return nil, err
	}
	var raw string
	if err := json.Unmarshal(result, &raw); err != nil {
		return nil, fmt.Errorf("解析 eth_getCode 结果: %w", err)
	}
	return common.FromHex(raw), nil
}

// callContract performs an eth_call and returns the raw return data.
func (c *rpcClient) callContract(ctx context.Context, to common.Address, selector string) ([]byte, error) {
	result, err := c.call(ctx, "eth_call", []any{
		map[string]any{"to": to.Hex(), "data": selector}, "latest",
	})
	if err != nil {
		return nil, err
	}
	var raw string
	if err := json.Unmarshal(result, &raw); err != nil {
		return nil, fmt.Errorf("解析 eth_call 结果: %w", err)
	}
	return common.FromHex(raw), nil
}

func (c *rpcClient) hasCode(ctx context.Context, address common.Address) (bool, error) {
	code, err := c.code(ctx, address)
	if err != nil {
		return false, err
	}
	return len(code) > 0, nil
}

// detectWalletType classifies the account wallet from its bytecode. It returns
// -1 when the bytecode is not recognised so the caller can fall back to the
// derivation that produced the address.
func detectWalletType(ctx context.Context, rpc *rpcClient, signer, funder common.Address, code []byte) int {
	switch {
	case len(code) == 0:
		return -1
	case bytes.HasPrefix(code, eip7702Prefix):
		// A delegated EOA executes deposit-wallet code through EIP-7702.
		return WalletTypeDeposit
	case bytes.HasPrefix(code, eip1167Prefix):
		// Legacy Polymarket Proxy is an EIP-1167 clone of its implementation.
		return WalletTypeProxy
	case bytes.Contains(code, beaconMarker), bytes.Contains(code, uupsMarker):
		// Deposit Wallet: Solady ERC-1967 beacon / UUPS clone with immutable args.
		return WalletTypeDeposit
	}
	if rpc != nil {
		// A Gnosis Safe answers getOwners() with a non-empty address array.
		if data, err := rpc.callContract(ctx, funder, "0xa0e67e2b"); err == nil && len(data) >= 64 {
			return WalletTypeSafe
		}
		// Deposit Wallet exposes owner() (single owner).
		if data, err := rpc.callContract(ctx, funder, "0x8da5cb5b"); err == nil && len(data) >= 32 {
			if common.BytesToAddress(data[12:32]) == signer {
				return WalletTypeDeposit
			}
		}
	}
	return -1
}

// discoverProfileWallet asks Gamma for the public profile of signer (or of an
// already-known wallet) and returns the account wallet it is bound to.
//
// A 404 means the address has never been linked to a polymarket.com account;
// that is not an error, it only means this source has nothing to offer.
func discoverProfileWallet(ctx context.Context, client *http.Client, gammaBaseURL, address string) (string, bool, error) {
	base := strings.TrimRight(strings.TrimSpace(gammaBaseURL), "/")
	if base == "" {
		return "", false, nil
	}
	target := base + profilePath + "?address=" + url.QueryEscape(address)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", false, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return "", false, fmt.Errorf("查询 Polymarket 公开资料: %w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", false, nil
	default:
		return "", false, fmt.Errorf("Polymarket 公开资料返回 %s", response.Status)
	}
	var profile struct {
		ProxyWallet string `json:"proxyWallet"`
	}
	if err := json.Unmarshal(body, &profile); err != nil {
		return "", false, fmt.Errorf("解析 Polymarket 公开资料: %w", err)
	}
	wallet := normalizeKeyPrefix(strings.TrimSpace(profile.ProxyWallet))
	if !common.IsHexAddress(wallet) {
		return "", false, nil
	}
	return wallet, true, nil
}

// DiscoverWallet resolves the Polymarket account wallet (the "funder") that
// belongs to the configured signer and reports its wallet type. It needs only
// the private key: the CLOB L1 credentials are not derived and nothing is
// signed or saved.
func (e *Executor) DiscoverWallet(ctx context.Context, settings engine.LiveSettings, endpoints engine.WalletEndpoints) (engine.WalletDiscovery, error) {
	if e == nil {
		return engine.WalletDiscovery{}, fmt.Errorf("未配置实盘执行器")
	}
	cfg, err := e.resolveSigner(settings, endpoints.CLOB)
	if err != nil {
		return engine.WalletDiscovery{}, err
	}
	privateKey, err := crypto.HexToECDSA(strings.TrimPrefix(cfg.PrivateKey, "0x"))
	if err != nil {
		return engine.WalletDiscovery{}, fmt.Errorf("POLYMARKET_PRIVATE_KEY 无效: %w", err)
	}
	signer := crypto.PubkeyToAddress(privateKey.PublicKey)
	result := engine.WalletDiscovery{Signer: signer.Hex()}

	var candidates []walletCandidate
	seen := map[common.Address]bool{}
	addCandidate := func(address common.Address, source walletSource) {
		if seen[address] {
			return
		}
		seen[address] = true
		candidates = append(candidates, walletCandidate{address: address, source: source})
	}

	// 1) Public profile: the authoritative owner → account-wallet mapping.
	profileClient := &http.Client{Timeout: 12 * time.Second}
	if cfg.Proxy != nil {
		profileClient = cfg.Proxy.HTTPClient(12 * time.Second)
	}
	if wallet, ok, err := discoverProfileWallet(ctx, profileClient, endpoints.Gamma, signer.Hex()); err != nil {
		result.Notes = append(result.Notes, err.Error())
	} else if ok {
		addCandidate(common.HexToAddress(wallet), sourceProfile)
	}

	// 2) CREATE2 derivation of the deterministic wallets, verified on-chain.
	// Order matters: the deposit wallet is the current account type, so it wins
	// over a legacy Proxy/Safe that may still be deployed for a migrated account.
	rpc := newRPCClient(cfg)
	derived := []struct {
		address common.Address
		kind    int
	}{
		{address: DeriveDepositAddress(signer), kind: WalletTypeDeposit},
		{address: DeriveSafeAddress(signer), kind: WalletTypeSafe},
		{address: DeriveProxyAddress(signer), kind: WalletTypeProxy},
	}
	rpcAvailable := true
	for _, candidate := range derived {
		deployed, err := rpc.hasCode(ctx, candidate.address)
		if err != nil {
			if rpcAvailable {
				result.Notes = append(result.Notes, "链上校验不可用："+err.Error())
				rpcAvailable = false
			}
			continue
		}
		if deployed {
			addCandidate(candidate.address, sourceDerivation)
		}
	}
	if len(candidates) == 0 {
		if !rpcAvailable {
			result.Notes = append(result.Notes, "无法通过链上校验寻找钱包：请检查网络出口，或先在 polymarket.com 用该钱包登录一次。")
		}
		result.Notes = append(result.Notes,
			"未找到该私钥对应的 Polymarket 账户钱包：请先在 polymarket.com 用这个私钥对应的钱包登录一次，让平台为它创建账户钱包（Deposit Wallet），或从个人菜单复制地址后手动填入。")
		return result, fmt.Errorf("未能自动找到 Funder：Polymarket 公开资料中没有该地址，链上也没有可识别的代理钱包")
	}

	// Resolve deployment state after de-duplication so the reported wallet has
	// an authoritative answer even when it came from the profile.
	if rpcAvailable {
		for i := range candidates {
			deployed, err := rpc.hasCode(ctx, candidates[i].address)
			if err != nil {
				continue
			}
			candidates[i].deployed = deployed
		}
	}

	primary := candidates[0]
	result.Funder = primary.address.Hex()
	result.Source = string(primary.source)
	result.Deployed = primary.deployed

	// Classify: a derivation match is exact, otherwise read the bytecode.
	walletType := -1
	if primary.address == signer {
		walletType = WalletTypeEOA
	}
	for _, candidate := range derived {
		if primary.address == candidate.address {
			walletType = candidate.kind
			break
		}
	}
	codeChecked := false
	if walletType < 0 && rpcAvailable {
		if code, err := rpc.code(ctx, primary.address); err == nil {
			codeChecked = len(code) > 0
			walletType = detectWalletType(ctx, rpc, signer, primary.address, code)
		}
	}
	if walletType < 0 {
		// Every Polymarket account wallet is a smart wallet; new accounts get a
		// Deposit Wallet and the profile service only lists existing accounts.
		walletType = WalletTypeDeposit
		if codeChecked {
			result.Notes = append(result.Notes, "链上字节码不是已知的钱包形态，已按默认的 Deposit Wallet（3）处理；如果订单被拒，请手动选择正确的代理钱包类型。")
		}
	}
	if len(candidates) > 1 && walletType != WalletTypeEOA {
		result.Notes = append(result.Notes, "该私钥关联了多个钱包，已按优先级填入第一个（Polymarket 公开资料优先）；如果订单被拒，请改用 polymarket.com 个人菜单里显示的账户钱包地址。")
	}
	result.WalletType = walletType
	result.WalletName = walletTypeName(walletType)

	if walletType == WalletTypeEOA {
		result.Funder = ""
		result.Wallets = nil
	} else {
		for _, candidate := range candidates {
			result.Wallets = append(result.Wallets, candidate.address.Hex())
			kind := -1
			for _, known := range derived {
				if candidate.address == known.address {
					kind = known.kind
					break
				}
			}
			if kind < 0 && rpcAvailable {
				if code, err := rpc.code(ctx, candidate.address); err == nil {
					kind = detectWalletType(ctx, rpc, signer, candidate.address, code)
				}
			}
			result.Candidates = append(result.Candidates, engine.WalletCandidate{
				Address:    candidate.address.Hex(),
				WalletType: kind,
				WalletName: walletTypeNameForCandidate(kind),
				Deployed:   candidate.deployed,
				Source:     string(candidate.source),
			})
		}
	}
	if walletType == WalletTypeDeposit && !result.Deployed && rpcAvailable {
		result.Notes = append(result.Notes, "Deposit 钱包尚未在链上部署：下单前需要在 polymarket.com 完成一次存款 / 交易，让平台部署该钱包。")
	}
	return result, nil
}

func walletTypeNameForCandidate(signatureType int) string {
	if signatureType < 0 {
		return "未识别"
	}
	return walletTypeName(signatureType)
}

// resolveSigner merges only the signer fields so wallet discovery is not
// blocked by an unknown or still-empty funder.
func (e *Executor) resolveSigner(settings engine.LiveSettings, clobBaseURL string) (Config, error) {
	settings = settings.Normalize()
	e.mu.Lock()
	cfg := e.base
	cfg.Proxy = e.cfg.Proxy
	e.mu.Unlock()
	if clob := strings.TrimRight(strings.TrimSpace(clobBaseURL), "/"); clob != "" {
		cfg.CLOBURL = clob
	}
	if settings.PrivateKey != "" {
		cfg.PrivateKey = settings.PrivateKey
	}
	cfg.SignatureType = 0
	cfg.Funder = ""
	cfg.APIKey, cfg.APISecret, cfg.APIPassphrase = "", "", ""
	if strings.TrimSpace(cfg.PrivateKey) == "" {
		return Config{}, fmt.Errorf("未配置 POLYMARKET_PRIVATE_KEY")
	}
	if _, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(cfg.PrivateKey), "0x")); err != nil {
		return Config{}, fmt.Errorf("POLYMARKET_PRIVATE_KEY 无效: %w", err)
	}
	return cfg, nil
}
