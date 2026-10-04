package recorder

import (
	"encoding/binary"
	"hash/fnv"
	"math"
	"time"

	"fomobot/internal/db"
	"fomobot/internal/fomo"
)

func fp(v fomo.FlexFloat) *float64 { f := v.Float(); return &f }
func ip(v fomo.FlexInt) *int       { i := int(v); return &i }

func feedThesis(e *fomo.Event, at time.Time) db.Thesis {
	b := &e.Body
	t := db.Thesis{
		ID:                  e.ThesisID(),
		TokenAddress:        e.TokenAddress,
		NetworkID:           e.NetworkID.String(),
		Ticker:              b.Ticker,
		UserID:              e.UserID.String(),
		Handle:              b.UserHandle,
		DisplayName:         b.DisplayName,
		UserImageURL:        b.UserImageURL,
		Verified:            bool(e.Verified),
		IsDev:               bool(b.IsDev),
		Twitter:             e.Twitter,
		CreatedAt:           e.CreatedAt.Time,
		TradeID:             e.TradeID.String(),
		SwapID:              e.SwapID.String(),
		TransferID:          e.TransferID.String(),
		FeedEventID:         e.ID.String(),
		Comment:             b.Comment,
		PriceAtCreation:     b.PriceUSDAtCreation.Float(),
		MarketCapAtCreation: b.MarketCapAtCreation.Float(),
		FDVAtCreation:       b.FDVAtCreation.Float(),
		PositionUSD:         fp(b.PositionNotionalUSD),
		HumanTokenAmount:    fp(b.HumanTokenAmount),
		RealizedPnl:         fp(b.RealizedPnlUSD),
		UnrealizedPnl:       fp(b.UnrealizedPnlUSD),
		PctRealizedPnl:      fp(b.PctRealizedPnl),
		PctUnrealizedPnl:    fp(b.PctUnrealizedPnl),
		Likes:               ip(e.Likes),
		Views:               ip(e.Views),
		NumReplies:          ip(e.NumReplies),
		TokenMarketCap:      fp(b.MarketCap),
		TokenPrice:          fp(b.Price),
		TokenFDV:            fp(b.FDV),
		Source:              db.SourceFeed,
		ObservedAt:          at,
		Raw:                 e.Raw,
	}
	if tc := e.TradeComment; tc != nil {
		t.OlderThesis = ip(tc.OlderThesis)
		t.NewerThesis = ip(tc.NewerThesis)
		if t.CreatedAt.IsZero() {
			t.CreatedAt = tc.CreatedAt.Time
		}
	}
	return t
}

func tokenThesis(r fomo.RawTokenThesis, at time.Time) db.Thesis {
	it := &r.Item
	t := db.Thesis{
		ID:                  it.ID.String(),
		TokenAddress:        it.TokenAddress,
		NetworkID:           it.NetworkID.String(),
		Ticker:              it.Ticker,
		UserID:              it.UserID.String(),
		Handle:              it.UserHandle,
		DisplayName:         it.DisplayName,
		UserImageURL:        it.UserImageURL,
		Verified:            bool(it.Verified),
		IsDev:               bool(it.IsDev),
		Twitter:             it.Twitter,
		CreatedAt:           it.CreatedAt.Time,
		TradeID:             it.TradeID.String(),
		Comment:             it.Comment.Comment,
		PriceAtCreation:     it.Comment.PriceUSDAtCreation.Float(),
		MarketCapAtCreation: it.Comment.MarketCapAtCreation.Float(),
		FDVAtCreation:       it.Comment.FDVAtCreation.Float(),
		Equity:              fp(it.Equity),
		Threshold:           fp(it.Threshold),
		Likes:               ip(it.Comment.NumLikes),
		NumReplies:          ip(it.NumReplies),
		OlderThesis:         ip(it.Comment.OlderThesis),
		NewerThesis:         ip(it.Comment.NewerThesis),
		Source:              db.SourceTokenThesis,
		ObservedAt:          at,
		Raw:                 r.JSON,
	}
	if a := it.AuthorTrade; a != nil {
		t.PositionUSD = fp(a.USDValue)
		t.HumanTokenAmount = fp(a.HumanTokenAmount)
		t.RealizedPnl = fp(a.RealizedPnlUSD)
		t.UnrealizedPnl = fp(a.UnrealizedPnlUSD)
		t.PctRealizedPnl = fp(a.PctRealizedPnl)
		t.PctUnrealizedPnl = fp(a.PctUnrealizedPnl)
		if !a.ClosedAt.IsZero() {
			closed := a.ClosedAt.Time
			t.ClosedAt = &closed
		}
	}
	return t
}

// stateHash covers the fields that change over time; an observation is only
// stored when it differs from the previous one of the same source.
func stateHash(t *db.Thesis) uint64 {
	h := fnv.New64a()
	var buf [8]byte
	putF := func(p *float64) {
		v := math.NaN()
		if p != nil {
			v = *p
		}
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(v))
		h.Write(buf[:])
	}
	putI := func(p *int) {
		v := int64(math.MinInt64)
		if p != nil {
			v = int64(*p)
		}
		binary.LittleEndian.PutUint64(buf[:], uint64(v))
		h.Write(buf[:])
	}
	for _, p := range []*float64{t.PositionUSD, t.HumanTokenAmount, t.RealizedPnl, t.UnrealizedPnl,
		t.TokenMarketCap, t.TokenPrice, t.Equity} {
		putF(p)
	}
	for _, p := range []*int{t.Likes, t.Views, t.NumReplies, t.OlderThesis, t.NewerThesis} {
		putI(p)
	}
	closed := int64(0)
	if t.ClosedAt != nil {
		closed = t.ClosedAt.UnixNano()
	}
	binary.LittleEndian.PutUint64(buf[:], uint64(closed))
	h.Write(buf[:])
	h.Write([]byte{byte(len(t.Handle)), boolByte(t.Verified)})
	h.Write([]byte(t.Handle))
	return h.Sum64()
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}
