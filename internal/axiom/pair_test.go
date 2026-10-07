package axiom

import (
	"strings"
	"testing"
	"time"
)

const sample = `{"room":"new_pairs","content":{"pair_address":"7ACwXXCiXDYyJdstfUCmiQmcxK4zjJVWnFMESk9XsfPG","signature":"3rLskxWSxquk1YkTdjb8SGFbBgR2kj4f5qmtysgf4k5KkoUuURcRQt9FmccSNMtkb53GsYjKxCd1Uv6ggZDqA4Ym","token_address":"A2Pfj8iAATQ8YetDdMTXveEvLt4MxjwUubsgyGkaJcZY","token_name":"ひきこもり","token_ticker":"HIKIKOMORI","token_image":null,"token_uri":"https://ipfs.io/ipfs/bafkreihyznikzjm3nbh7sx4r2c7d6kkuuftanq2mslzuhg4iazeafujyf4","token_decimals":6,"pair_sol_account":"AM8bLVvRxg3VXDUDyadApPXVGtzCMppR7hJVpgxN12U9","pair_token_account":"5pqCSxu3hG2tm4rU3Uoa6HsrrtJYVvWxYZ7VXNkPwHpP","protocol":"Virtual Curve","protocol_details":{"quote_mint":"So11111111111111111111111111111111111111112","created_at":1790684911625,"curve":[{"sqrt_price":"0","liquidity":"0"}],"isUsdc":false},"created_at":"2026-09-29T12:28:31.625Z","website":null,"twitter":null,"telegram":null,"discord":null,"mint_authority":null,"open_trading":"2026-09-29T12:27:31.625Z","deployer_address":"FWudqzqpPGCjr11gDhn3AMbmnQq46oV6u56WBjyrdBpw","supply":1000000000,"initial_liquidity_sol":30,"initial_liquidity_token":5.92744940995599e-07,"top_10_holders":0,"lp_burned":11,"updated_at":"2026-09-29T12:28:31.625Z","freeze_authority":null,"extra":null,"slot":451641903,"display_protocol":"Virtual Curve"}}`

func TestParseNewPair(t *testing.T) {
	p, ok := Parse([]byte(sample))
	if !ok {
		t.Fatal("sample not parsed")
	}
	want := Pair{
		Token: "A2Pfj8iAATQ8YetDdMTXveEvLt4MxjwUubsgyGkaJcZY", Pair: "7ACwXXCiXDYyJdstfUCmiQmcxK4zjJVWnFMESk9XsfPG",
		Name: "ひきこもり", Ticker: "HIKIKOMORI", Protocol: "Virtual Curve", DisplayProtocol: "Virtual Curve",
		Deployer:     "FWudqzqpPGCjr11gDhn3AMbmnQq46oV6u56WBjyrdBpw",
		LiquiditySOL: 30,
		CreatedAt:    time.Date(2026, 9, 29, 12, 28, 31, 625_000_000, time.UTC),
	}
	if !p.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("created = %v, want %v", p.CreatedAt, want.CreatedAt)
	}
	p.CreatedAt = want.CreatedAt
	if p != want {
		t.Errorf("pair = %+v\nwant   %+v", p, want)
	}
}

func TestParseSkipsOtherMessages(t *testing.T) {
	for _, raw := range []string{
		`{"room":"sol_price","content":151.2}`,
		`{"room":"new_pairs","content":{"pair_address":"P"}}`,
		`{"room":"new_pairs","content":"oops"}`,
		`not json`,
	} {
		if _, ok := Parse([]byte(raw)); ok {
			t.Errorf("%s must be skipped", raw)
		}
	}
	noTime := strings.Replace(sample, `"created_at":"2026-09-29T12:28:31.625Z"`, `"created_at":null`, 1)
	if p, ok := Parse([]byte(noTime)); !ok || !p.CreatedAt.IsZero() {
		t.Errorf("missing created_at: ok=%v created=%v", ok, p.CreatedAt)
	}
}

func TestScriptHasNoPlaceholders(t *testing.T) {
	s := (&Stream{cfg: Config{Clusters: []string{"wss://a/", "wss://b/"}}}).script()
	if strings.Contains(s, "__CLUSTERS__") || strings.Contains(s, "__FRAME__") || !strings.Contains(s, `["wss://a/","wss://b/"]`) ||
		!strings.Contains(s, `window.`+frameBinding+`(s)`) {
		t.Errorf("script:\n%s", s)
	}
}
