package predictfun

// This file is generated from the official SDK (src/Constants.ts,
// AddressesByChainId). Do not hand-edit from documentation: the Predict.fun
// docs site was compromised once and is not an authoritative source. The SDK
// source is.
//
// ConditionalTokens is the ERC-1155 holding outcome shares (needs
// setApprovalForAll for the exchange); USDT is the collateral (needs an ERC-20
// allowance). Both are checked before the first live order.
//
// The YieldBearing* track is a separate contract family this build does not
// sign for. Markets flagged isYieldBearing are refused rather than routed to
// the wrong exchange.
type Addresses struct {
	Exchange                             string
	NegRiskExchange                      string
	NegRiskAdapter                       string
	ConditionalTokens                    string
	NegRiskConditionalTokens             string
	USDT                                 string
	Kernel                               string
	ECDSAValidator                       string
	YieldBearingExchange                 string
	YieldBearingNegRiskExchange          string
	YieldBearingNegRiskAdapter           string
	YieldBearingConditionalTokens        string
	YieldBearingNegRiskConditionalTokens string
}

const (
	// ChainIDBnbMainnet is BNB Smart Chain.
	ChainIDBnbMainnet int64 = 56
	// ChainIDBnbTestnet is the BNB test chain Predict.fun's testnet API uses.
	ChainIDBnbTestnet int64 = 97

	// ProtocolName and ProtocolVersion are EIP-712 domain fields. They are not
	// configurable: a different name signs a different hash and the exchange
	// rejects the order.
	ProtocolName    = "predict.fun CTF Exchange"
	ProtocolVersion = "1"

	// MaxSalt bounds the randomly drawn order salt, mirroring the SDK.
	MaxSalt = 2_147_483_648
)

// Side is the order direction in the signed struct.
type Side uint8

const (
	SideBuy  Side = 0
	SideSell Side = 1
)

// SignatureType is how the exchange validates an order signature. This build
// only signs EOA orders; the smart-wallet wrapper needs the Kernel domain and a
// validator contract call, which is not implemented.
type SignatureType uint8

const SignatureTypeEOA SignatureType = 0

var (
	mainnetAddresses = Addresses{
		Exchange:                             "0x8BC070BEdAB741406F4B1Eb65A72bee27894B689",
		NegRiskExchange:                      "0x365fb81bd4A24D6303cd2F19c349dE6894D8d58A",
		NegRiskAdapter:                       "0xc3Cf7c252f65E0d8D88537dF96569AE94a7F1A6E",
		ConditionalTokens:                    "0x22DA1810B194ca018378464a58f6Ac2B10C9d244",
		NegRiskConditionalTokens:             "0x22DA1810B194ca018378464a58f6Ac2B10C9d244",
		USDT:                                 "0x55d398326f99059fF775485246999027B3197955",
		Kernel:                               "0xBAC849bB641841b44E965fB01A4Bf5F074f84b4D",
		ECDSAValidator:                       "0x845ADb2C711129d4f3966735eD98a9F09fC4cE57",
		YieldBearingExchange:                 "0x6bEb5a40C032AFc305961162d8204CDA16DECFa5",
		YieldBearingNegRiskExchange:          "0x8A289d458f5a134bA40015085A8F50Ffb681B41d",
		YieldBearingNegRiskAdapter:           "0x41dCe1A4B8FB5e6327701750aF6231B7CD0B2A40",
		YieldBearingConditionalTokens:        "0x9400F8Ad57e9e0F352345935d6D3175975eb1d9F",
		YieldBearingNegRiskConditionalTokens: "0xF64b0b318AAf83BD9071110af24D24445719A07F",
	}
	testnetAddresses = Addresses{
		Exchange:                             "0x2A6413639BD3d73a20ed8C95F634Ce198ABbd2d7",
		NegRiskExchange:                      "0xd690b2bd441bE36431F6F6639D7Ad351e7B29680",
		NegRiskAdapter:                       "0x285c1B939380B130D7EBd09467b93faD4BA623Ed",
		ConditionalTokens:                    "0x2827AAef52D71910E8FBad2FfeBC1B6C2DA37743",
		NegRiskConditionalTokens:             "0x2827AAef52D71910E8FBad2FfeBC1B6C2DA37743",
		USDT:                                 "0xB32171ecD878607FFc4F8FC0bCcE6852BB3149E0",
		Kernel:                               "0xBAC849bB641841b44E965fB01A4Bf5F074f84b4D",
		ECDSAValidator:                       "0x845ADb2C711129d4f3966735eD98a9F09fC4cE57",
		YieldBearingExchange:                 "0x8a6B4Fa700A1e310b106E7a48bAFa29111f66e89",
		YieldBearingNegRiskExchange:          "0x95D5113bc50eD201e319101bbca3e0E250662fCC",
		YieldBearingNegRiskAdapter:           "0xb74aea04bdeBE912Aa425bC9173F9668e6f11F99",
		YieldBearingConditionalTokens:        "0x38BF1cbD66d174bb5F3037d7068E708861D68D7f",
		YieldBearingNegRiskConditionalTokens: "0x26e865CbaAe99b62fbF9D18B55c25B5E079A93D5",
	}
)

// AddressesFor returns the contract set for a chain id. Any chain other than
// BNB mainnet or testnet is a configuration error, never a fallback to
// mainnet: signing against the wrong chain wastes gas and could not settle.
func AddressesFor(chainID int64) (Addresses, bool) {
	switch chainID {
	case ChainIDBnbMainnet:
		return mainnetAddresses, true
	case ChainIDBnbTestnet:
		return testnetAddresses, true
	}
	return Addresses{}, false
}
