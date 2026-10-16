package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/store"
)

// Data-transfer unit prices share price_cache with on-demand and EBS
// prices: one row per (region, class, direction), os 'TRANSFER',
// instance_type '<class>:<rx|tx>' ('internet:tx', 'zone:rx',
// 'provider:us-east-1:tx'), the price per GB in per_hour, and the public
// IPv4 hourly charge as 'public-ipv4:hour'. 'internet:tx' is always present
// for a fetched set, so its fetched_at dates the set.
const transferCacheOS = "TRANSFER"

const transferAnchor = "internet:tx"

// rows are p as price_cache rows: instance_type to price.
func (p DataTransferPrices) rows() map[string]string {
	out := map[string]string{}
	put := func(key string, g PerGB) {
		if g.Rx != "" {
			out[key+":rx"] = g.Rx
		}
		if g.Tx != "" {
			out[key+":tx"] = g.Tx
		}
	}
	put("internet", p.Internet)
	put("zone", p.Zone)
	put("region-public", p.RegionPublic)
	for r, g := range p.Provider {
		put("provider:"+r, g)
	}
	if p.PublicIPv4PerHour != "" {
		out["public-ipv4:hour"] = p.PublicIPv4PerHour
	}
	return out
}

// set is rows' inverse for one row.
func (p *DataTransferPrices) set(key, v string) {
	if key == "public-ipv4:hour" {
		p.PublicIPv4PerHour = v
		return
	}
	i := strings.LastIndexByte(key, ':')
	if i < 0 {
		return
	}
	class, dir := key[:i], key[i+1:]
	setDir := func(g PerGB) PerGB {
		switch dir {
		case "rx":
			g.Rx = v
		case "tx":
			g.Tx = v
		}
		return g
	}
	switch {
	case class == "internet":
		p.Internet = setDir(p.Internet)
	case class == "zone":
		p.Zone = setDir(p.Zone)
	case class == "region-public":
		p.RegionPublic = setDir(p.RegionPublic)
	case strings.HasPrefix(class, "provider:"):
		if p.Provider == nil {
			p.Provider = map[string]PerGB{}
		}
		r := strings.TrimPrefix(class, "provider:")
		p.Provider[r] = setDir(p.Provider[r])
	}
}

// validDataTransfer: a currency, an internet price, and every price a
// non-negative decimal of at most 9 fractional digits.
func validDataTransfer(p DataTransferPrices) error {
	if p.Currency == "" || p.Internet.Tx == "" {
		return errors.New("no internet transfer price")
	}
	for k, v := range p.rows() {
		if err := validPrice(v, p.Currency); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

func trimmedDataTransfer(p DataTransferPrices) DataTransferPrices {
	out := DataTransferPrices{Currency: p.Currency}
	for k, v := range p.rows() {
		out.set(k, trimmedDecimal(v))
	}
	return out
}

func cachedDataTransfer(ctx context.Context, tx pgx.Tx, provider, region string) (DataTransferPrices, time.Time, error) {
	var price DataTransferPrices
	var fetched time.Time
	rows, err := tx.Query(ctx, `SELECT instance_type, trim_scale(per_hour)::text, currency, fetched_at
		FROM price_cache WHERE provider = $1 AND region = $2 AND os = $3`, provider, region, transferCacheOS)
	if err != nil {
		return price, fetched, err
	}
	var key, v, currency string
	var at time.Time
	_, err = pgx.ForEachRow(rows, []any{&key, &v, &currency, &at}, func() error {
		price.set(key, v)
		if key == transferAnchor {
			price.Currency, fetched = currency, at
		}
		return nil
	})
	if price.Currency == "" {
		return DataTransferPrices{}, time.Time{}, err
	}
	return price, fetched, err
}

// dataTransferPrice is a region's transfer prices: the cache while fresh,
// else the provider's answer (cached), else a stale cached answer, as
// blockStoragePrice does.
func (s *Server) dataTransferPrice(ctx context.Context, provider, region string) (DataTransferPrices, error) {
	var cached DataTransferPrices
	var fetched time.Time
	if err := s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		var err error
		cached, fetched, err = cachedDataTransfer(ctx, tx, provider, region)
		return err
	}); err != nil {
		return cached, err
	}
	if cached.Currency != "" && time.Since(fetched) < s.cfg.Costs.PricesRefresh {
		return cached, nil
	}
	p := s.cfg.Costs.Prices[provider]
	if p == nil {
		if cached.Currency != "" {
			return cached, nil
		}
		return cached, fmt.Errorf("no price provider for %s", provider)
	}
	price, err := p.DataTransfer(ctx, region)
	if err == nil {
		price = trimmedDataTransfer(price)
		err = validDataTransfer(price)
	}
	if err != nil {
		if cached.Currency != "" {
			return cached, nil
		}
		return DataTransferPrices{}, fmt.Errorf("%s data transfer price %s: %w", provider, region, err)
	}
	err = s.db.Tx(ctx, store.System(), func(tx pgx.Tx) error {
		// Keys in sorted order, so two luxds refreshing one region take the
		// row locks alike; keys the new answer lacks are deleted.
		rows := price.rows()
		if _, err := tx.Exec(ctx, `DELETE FROM price_cache WHERE provider = $1 AND region = $2 AND os = $3 AND instance_type <> ALL($4)`,
			provider, region, transferCacheOS, slices.Sorted(maps.Keys(rows))); err != nil {
			return err
		}
		for _, k := range slices.Sorted(maps.Keys(rows)) {
			if _, err := tx.Exec(ctx, `INSERT INTO price_cache (provider, region, instance_type, os, per_hour, currency, fetched_at)
				VALUES ($1, $2, $3, $4, $5, $6, clock_timestamp()) ON CONFLICT (provider, region, instance_type, os)
				DO UPDATE SET per_hour = EXCLUDED.per_hour, currency = EXCLUDED.currency, fetched_at = EXCLUDED.fetched_at`,
				provider, region, k, transferCacheOS, rows[k], price.Currency); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return DataTransferPrices{}, err
	}
	return price, nil
}
