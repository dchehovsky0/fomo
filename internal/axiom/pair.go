// Package axiom follows the new_pairs room of Axiom's WebSocket: every new
// Solana pool with its token already parsed.
package axiom

import (
	"encoding/json"
	"time"
)

const Room = "new_pairs"

// Pair is a new pool; Token is the new token's mint.
type Pair struct {
	Token           string
	Pair            string
	Name            string
	Ticker          string
	Protocol        string
	DisplayProtocol string
	Deployer        string
	// CreatedAt is zero when Axiom sent none or in an unknown format.
	CreatedAt time.Time
}

// Label is the protocol name as Axiom shows it.
func (p Pair) Label() string {
	if p.DisplayProtocol != "" {
		return p.DisplayProtocol
	}
	return p.Protocol
}

type wsMessage struct {
	Room    string          `json:"room"`
	Content json.RawMessage `json:"content"`
}

type wsPair struct {
	PairAddress     string `json:"pair_address"`
	TokenAddress    string `json:"token_address"`
	TokenName       string `json:"token_name"`
	TokenTicker     string `json:"token_ticker"`
	Protocol        string `json:"protocol"`
	DisplayProtocol string `json:"display_protocol"`
	Deployer        string `json:"deployer_address"`
	CreatedAt       string `json:"created_at"`
}

// Parse decodes a new_pairs message. ok is false for other rooms and for
// messages without a token.
func Parse(data []byte) (Pair, bool) {
	var m wsMessage
	if err := json.Unmarshal(data, &m); err != nil || m.Room != Room {
		return Pair{}, false
	}
	var p wsPair
	if err := json.Unmarshal(m.Content, &p); err != nil || p.TokenAddress == "" {
		return Pair{}, false
	}
	created, _ := time.Parse(time.RFC3339Nano, p.CreatedAt)
	return Pair{
		Token: p.TokenAddress, Pair: p.PairAddress, Name: p.TokenName, Ticker: p.TokenTicker,
		Protocol: p.Protocol, DisplayProtocol: p.DisplayProtocol, Deployer: p.Deployer,
		CreatedAt: created,
	}, true
}
