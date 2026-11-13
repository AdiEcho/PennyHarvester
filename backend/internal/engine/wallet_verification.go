package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"pennyharvester/backend/internal/market"
)

const (
	walletCacheTTL   = 10 * time.Minute
	walletCacheLimit = 64
)

// walletCacheEntry is shared by successful discovery results and proxy-funder
// verification results. It is intentionally small: the cache must never become
// a second source of live credentials.
type walletCacheEntry struct {
	expires    time.Time
	signer     string
	funder     string
	walletType int
	chainID    int64
	owner      string
	evidence   string
	discovery  *WalletDiscovery
}

// verifyProxyFunderForSave proves that a non-empty Polymarket funder belongs to
// the candidate signer before the candidate can be applied or persisted. A
// plain EOA has no funder and skips the check. Successful results are cached so
// saving the same verified wallet again does not make another RPC round trip.
func (e *Engine) verifyProxyFunderForSave(ctx context.Context, candidate SystemSettings) error {
	pm, ok := candidate.Venues[market.VenuePolymarket]
	if !ok {
		return nil
	}
	settings := pm.Live.LiveSettings
	funder := normalizeWalletAddress(settings.Funder)
	if funder == "" {
		return nil
	}
	walletType := e.effectiveWalletSignatureType(settings.SignatureType)
	if walletType == 0 {
		return fmt.Errorf("%w: 已填写 Funder %s，但有效签名类型为 EOA（0）：无法证明该代理/存款钱包归属于签名账户，请将签名类型设置为 1/2/3 或清空 Funder", ErrInvalidSettings, settings.Funder)
	}

	signer := e.walletVerificationSigner(candidate)
	if signer != "" {
		base := walletCacheBase("verify", signer, funder, 137, walletType)
		if _, ok := e.walletCacheFind(base+"|", func(entry walletCacheEntry) bool {
			return entry.signer == signer && entry.funder == funder &&
				entry.chainID == 137 && entry.walletType == walletType
		}); ok {
			return nil
		}
	}

	discovery, err := e.DiscoverWallet(ctx, market.VenuePolymarket, settings, WalletEndpoints{
		Gamma: candidate.DataSource.GammaBaseURL,
		CLOB:  candidate.DataSource.CLOBBaseURL,
	})
	if err != nil {
		return fmt.Errorf("%w: 无法验证 Polymarket 代理/存款钱包归属：%v", ErrInvalidSettings, err)
	}
	if !discovery.Verified || discovery.ChainID != 137 ||
		!strings.EqualFold(discovery.Funder, settings.Funder) || discovery.WalletType != walletType {
		return fmt.Errorf("%w: 无法证明 Funder %s 的代理/存款钱包归属于签名账户（verified=%t，chain_id=%d，wallet_type=%d，期望类型=%d）", ErrInvalidSettings, settings.Funder, discovery.Verified, discovery.ChainID, discovery.WalletType, walletType)
	}

	if signer == "" {
		signer = walletSignerIdentity("", discovery.Signer)
	}
	if signer != "" {
		base := walletCacheBase("verify", signer, funder, discovery.ChainID, walletType)
		e.walletCachePut(walletCacheKey(base, discovery.Evidence, discovery.Owner), walletCacheEntry{
			expires:    time.Now().Add(walletCacheTTL),
			signer:     signer,
			funder:     funder,
			walletType: walletType,
			chainID:    discovery.ChainID,
			owner:      normalizeWalletAddress(discovery.Owner),
			evidence:   strings.TrimSpace(discovery.Evidence),
		})
	}
	return nil
}

// effectiveWalletSignatureType uses the candidate's explicit type when present,
// otherwise the currently applied runtime type. It deliberately does not see a
// client-supplied "verified" flag.
func (e *Engine) effectiveWalletSignatureType(explicit *int) int {
	if explicit != nil {
		return *explicit
	}
	exec, err := e.executor(market.VenuePolymarket)
	if err != nil || exec == nil {
		return 0
	}
	live, ok := exec.(liveSettingsRuntime)
	if !ok || live == nil {
		return 0
	}
	return live.Environment().SignatureType
}

// walletVerificationSigner binds a save to the signer that will actually be
// used. With no candidate private key, the environment key is not used for the
// lookup until the previous stored key is also absent; otherwise clearing a
// stored key could reuse a result verified for that old key.
func (e *Engine) walletVerificationSigner(candidate SystemSettings) string {
	pm := candidate.Venues[market.VenuePolymarket]
	if signer := walletSignerIdentity(pm.Live.PrivateKey, ""); signer != "" {
		return signer
	}
	previous := e.SystemSettings().Venues[market.VenuePolymarket]
	if strings.TrimSpace(previous.Live.PrivateKey) != "" {
		return ""
	}
	exec, err := e.executor(market.VenuePolymarket)
	if err != nil || exec == nil {
		return ""
	}
	return walletSignerIdentity("", exec.Signer())
}

func (e *Engine) cachedWalletDiscovery(signer string, settings LiveSettings) (WalletDiscovery, bool) {
	if signer == "" {
		return WalletDiscovery{}, false
	}
	funder := normalizeWalletAddress(settings.Funder)
	walletType := -1
	if settings.SignatureType != nil {
		walletType = *settings.SignatureType
	}
	entry, ok := e.walletCacheFind("discover|"+signer+"|", func(entry walletCacheEntry) bool {
		if entry.discovery == nil || !entry.discovery.Verified || entry.discovery.ChainID != 137 {
			return false
		}
		if funder != "" && entry.funder != funder {
			return false
		}
		if walletType >= 0 && entry.walletType != walletType {
			return false
		}
		return true
	})
	if !ok || entry.discovery == nil {
		return WalletDiscovery{}, false
	}
	return cloneWalletDiscovery(*entry.discovery), true
}

func (e *Engine) cacheWalletDiscovery(signer string, result WalletDiscovery) {
	if signer == "" || !result.Verified || result.ChainID != 137 {
		return
	}
	funder := normalizeWalletAddress(result.Funder)
	owner := normalizeWalletAddress(result.Owner)
	evidence := strings.TrimSpace(result.Evidence)
	cached := cloneWalletDiscovery(result)
	base := walletCacheBase("discover", signer, funder, result.ChainID, result.WalletType)
	e.walletCachePut(walletCacheKey(base, evidence, owner), walletCacheEntry{
		expires:    time.Now().Add(walletCacheTTL),
		signer:     signer,
		funder:     funder,
		walletType: result.WalletType,
		chainID:    result.ChainID,
		owner:      owner,
		evidence:   evidence,
		discovery:  &cached,
	})
}

func (e *Engine) walletCacheFind(prefix string, accept func(walletCacheEntry) bool) (walletCacheEntry, bool) {
	e.walletCacheMu.Lock()
	defer e.walletCacheMu.Unlock()
	now := time.Now()
	for key, entry := range e.walletCache {
		if !now.Before(entry.expires) {
			delete(e.walletCache, key)
			continue
		}
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if accept(entry) {
			return entry, true
		}
	}
	return walletCacheEntry{}, false
}

func (e *Engine) walletCachePut(key string, entry walletCacheEntry) {
	if key == "" || entry.expires.IsZero() {
		return
	}
	e.walletCacheMu.Lock()
	defer e.walletCacheMu.Unlock()
	if e.walletCache == nil {
		e.walletCache = make(map[string]walletCacheEntry)
	}
	now := time.Now()
	for existing, cached := range e.walletCache {
		if !now.Before(cached.expires) {
			delete(e.walletCache, existing)
		}
	}
	if len(e.walletCache) >= walletCacheLimit {
		var oldestKey string
		var oldest time.Time
		for existing, cached := range e.walletCache {
			if oldestKey == "" || cached.expires.Before(oldest) {
				oldestKey, oldest = existing, cached.expires
			}
		}
		if oldestKey != "" {
			delete(e.walletCache, oldestKey)
		}
	}
	e.walletCache[key] = entry
}

func walletCacheBase(kind, signer, funder string, chainID int64, walletType int) string {
	return fmt.Sprintf("%s|%s|%s|%d|%d", kind, signer, funder, chainID, walletType)
}

func walletCacheKey(base, evidence, owner string) string {
	return base + "|" + normalizeWalletAddress(owner) + "|" + strings.TrimSpace(evidence)
}

// walletSignerIdentity avoids putting a private key itself into map keys. An
// explicit key identifies its signer; discovery results may also supply the
// signer address when the process environment holds the key.
func walletSignerIdentity(privateKey, signerAddress string) string {
	key := strings.TrimPrefix(strings.TrimSpace(privateKey), "0x")
	key = strings.TrimPrefix(key, "0X")
	if key != "" {
		sum := sha256.Sum256([]byte(strings.ToLower(key)))
		return "key:" + hex.EncodeToString(sum[:])
	}
	address := normalizeWalletAddress(signerAddress)
	if address == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(address))
	return "addr:" + hex.EncodeToString(sum[:])
}

func normalizeWalletAddress(address string) string {
	return strings.ToLower(strings.TrimSpace(address))
}

func cloneWalletDiscovery(in WalletDiscovery) WalletDiscovery {
	out := in
	out.Wallets = append([]string(nil), in.Wallets...)
	out.Candidates = append([]WalletCandidate(nil), in.Candidates...)
	out.Notes = append([]string(nil), in.Notes...)
	return out
}
