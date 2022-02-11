// Copyright 2020 Wayback Archiver. All rights reserved.
// Use of this source code is governed by the GNU GPL v3
// license that can be found in the LICENSE file.

package wayback // import "github.com/wabarc/wayback"

import (
	"context"
	"fmt"
	"net/url"
	"sync"

	"github.com/wabarc/logger"
	"github.com/wabarc/playback"
	"github.com/wabarc/rivet/ipfs"
	"github.com/wabarc/screenshot"
	"github.com/wabarc/wayback/config"
	"github.com/wabarc/wayback/errors"
	"github.com/wabarc/wayback/reduxer"
	"golang.org/x/sync/errgroup"

	is "github.com/wabarc/archive.is"
	ia "github.com/wabarc/archive.org"
	ip "github.com/wabarc/rivet"
	ph "github.com/wabarc/telegra.ph"

	pinner "github.com/wabarc/ipfs-pinner"
)

// Collect results that archived, Arc is name of the archive service,
// Dst mapping the original URL and archived destination URL,
// Ext is extra descriptions.
type Collect struct {
	Arc string // Archive slot name, see config/config.go
	Dst string // Archived destination URL
	Src string // Source URL
	Ext string // Extra identifier
}

// IA represents the Internet Archive slot.
type IA struct {
	URL *url.URL
	ctx context.Context
}

// IS represents the archive.today slot.
type IS struct {
	ctx context.Context

	URL *url.URL
}

// IP represents the IPFS slot.
type IP struct {
	ctx    context.Context
	bundle *reduxer.Bundle

	URL *url.URL
}

// PH represents the Telegra.ph slot.
type PH struct {
	ctx    context.Context
	bundle *reduxer.Bundle

	URL *url.URL
}

// Waybacker is the interface that wraps the basic Wayback method.
//
// Wayback wayback *url.URL from struct of the implementations to the Wayback Machine.
// It returns the result of string from the upstream services.
type Waybacker interface {
	Wayback() string
}

// Wayback implements the standard Waybacker interface:
// it reads URL from the IA and returns archived URL as a string.
func (i IA) Wayback() string {
	arc := &ia.Archiver{}
	dst, err := arc.Wayback(i.ctx, i.URL)
	if err != nil {
		logger.Error("wayback %s to Internet Archive failed: %v", i.URL.String(), err)
		return fmt.Sprint(err)
	}
	return dst
}

// Wayback implements the standard Waybacker interface:
// it reads URL from the IS and returns archived URL as a string.
func (i IS) Wayback() string {
	arc := &is.Archiver{}
	dst, err := arc.Wayback(i.ctx, i.URL)
	if err != nil {
		logger.Error("wayback %s to archive.today failed: %v", i.URL.String(), err)
		return fmt.Sprint(err)
	}
	return dst
}

// Wayback implements the standard Waybacker interface:
// it reads URL from the IP and returns archived URL as a string.
func (i IP) Wayback() string {
	opts := []ipfs.PinningOption{
		ipfs.Mode(ipfs.Remote),
	}
	if config.Opts.IPFSMode() == "daemon" {
		opts = []ipfs.PinningOption{
			ipfs.Mode(ipfs.Local),
			ipfs.Host(config.Opts.IPFSHost()),
			ipfs.Port(config.Opts.IPFSPort()),
		}
	}

	target := config.Opts.IPFSTarget()
	switch target {
	case pinner.Infura, pinner.Pinata, pinner.NFTStorage, pinner.Web3Storage:
		apikey := config.Opts.IPFSApikey()
		secret := config.Opts.IPFSSecret()
		opts = append(opts, ipfs.Uses(target), ipfs.Apikey(apikey), ipfs.Secret(secret))
	}

	arc := &ip.Shaft{Hold: ipfs.Options(opts...)}

	// If there is bundled HTML, it is utilized as the basis for IPFS
	// archiving and is sent to obelisk to crawl the rest of the page.
	if i.bundle != nil {
		i.ctx = arc.WithInput(i.ctx, i.bundle.HTML)
	}

	dst, err := arc.Wayback(i.ctx, i.URL)
	if err != nil {
		logger.Error("wayback %s to IPFS failed: %v", i.URL.String(), err)
		return fmt.Sprint(err)
	}
	return dst
}

// Wayback implements the standard Waybacker interface:
// it reads URL from the PH and returns archived URL as a string.
func (i PH) Wayback() string {
	arc := &ph.Archiver{}
	arc.SetShot(i.parseShot())
	if config.Opts.EnabledChromeRemote() {
		arc.ByRemote(config.Opts.ChromeRemoteAddr())
	}

	dst, err := arc.Wayback(i.ctx, i.URL)
	if err != nil {
		logger.Error("wayback %s to telegra.ph failed: %v", i.URL.String(), err)
		return fmt.Sprint(err)
	}
	return dst
}

func (i PH) parseShot() (shot screenshot.Screenshots) {
	if i.bundle != nil {
		shot = screenshot.Screenshots{
			URL:   i.bundle.URL,
			Title: i.bundle.Title,
			Image: i.bundle.Image,
			HTML:  i.bundle.HTML,
			PDF:   i.bundle.PDF,
		}
	}
	return
}

func wayback(w Waybacker) string {
	return w.Wayback()
}

// Wayback returns URLs archived to the time capsules of given URLs.
func Wayback(ctx context.Context, bundles *reduxer.Bundles, urls ...*url.URL) (cols []Collect, err error) {
	logger.Debug("start...")

	ctx, cancel := context.WithTimeout(ctx, config.Opts.WaybackTimeout())
	defer cancel()

	*bundles, err = reduxer.Do(ctx, urls...)
	if err != nil {
		logger.Warn("cannot to start reduxer: %v", err)
	}

	mu := sync.Mutex{}
	g, ctx := errgroup.WithContext(ctx)
	for _, input := range urls {
		for slot, arc := range config.Opts.Slots() {
			if !arc {
				logger.Warn("skipped %s", config.SlotName(slot))
				continue
			}
			slot, input := slot, input
			g.Go(func() error {
				logger.Debug("archiving slot: %s", slot)

				uri := input.String()
				bundle := bundles.Get(uri)
				var col Collect
				switch slot {
				case config.SLOT_IA:
					col.Dst = wayback(IA{URL: input, ctx: ctx})
				case config.SLOT_IS:
					col.Dst = wayback(IS{URL: input, ctx: ctx})
				case config.SLOT_IP:
					col.Dst = wayback(IP{URL: input, ctx: ctx, bundle: bundle})
				case config.SLOT_PH:
					col.Dst = wayback(PH{URL: input, ctx: ctx, bundle: bundle})
				}
				col.Src = uri
				col.Arc = slot
				col.Ext = slot
				mu.Lock()
				cols = append(cols, col)
				mu.Unlock()
				return nil
			})
		}
	}
	if err := g.Wait(); err != nil {
		logger.Error("archives failed: %v", err)
		return cols, err
	}

	if len(cols) == 0 {
		logger.Error("archives failure")
		return cols, errors.New("archives failure")
	}

	return cols, nil
}

// Playback returns URLs archived from the time capsules.
func Playback(ctx context.Context, urls ...*url.URL) (cols []Collect, err error) {
	logger.Debug("start...")

	ctx, cancel := context.WithTimeout(ctx, config.Opts.WaybackTimeout())
	defer cancel()

	mu := sync.Mutex{}
	g, ctx := errgroup.WithContext(ctx)
	var slots = []string{config.SLOT_IA, config.SLOT_IS, config.SLOT_IP, config.SLOT_PH, config.SLOT_TT, config.SLOT_GC}
	for _, input := range urls {
		for _, slot := range slots {
			slot, input := slot, input
			g.Go(func() error {
				logger.Debug("searching slot: %s", slot)
				var col Collect
				switch slot {
				case config.SLOT_IA:
					col.Dst = playback.Playback(ctx, playback.IA{URL: input})
				case config.SLOT_IS:
					col.Dst = playback.Playback(ctx, playback.IS{URL: input})
				case config.SLOT_IP:
					col.Dst = playback.Playback(ctx, playback.IP{URL: input})
				case config.SLOT_PH:
					col.Dst = playback.Playback(ctx, playback.PH{URL: input})
				case config.SLOT_TT:
					col.Dst = playback.Playback(ctx, playback.TT{URL: input})
				case config.SLOT_GC:
					col.Dst = playback.Playback(ctx, playback.GC{URL: input})
				}
				col.Src = input.String()
				col.Arc = slot
				col.Ext = slot
				mu.Lock()
				cols = append(cols, col)
				mu.Unlock()
				return nil
			})
		}
	}
	if err := g.Wait(); err != nil {
		logger.Error("failed: %v", err)
		return cols, err
	}

	return cols, nil
}
