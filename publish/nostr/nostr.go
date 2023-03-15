// Copyright 2023 Wayback Archiver. All rights reserved.
// Use of this source code is governed by the GNU GPL v3
// license that can be found in the LICENSE file.

package nostr // import "github.com/wabarc/wayback/publish/nostr"

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/wabarc/logger"
	"github.com/wabarc/wayback"
	"github.com/wabarc/wayback/config"
	"github.com/wabarc/wayback/errors"
	"github.com/wabarc/wayback/metrics"
	"github.com/wabarc/wayback/publish"
	"github.com/wabarc/wayback/reduxer"
	"github.com/wabarc/wayback/template/render"
	"golang.org/x/sync/errgroup"
)

const timeout = 10 * time.Second

// Interface guard
var _ publish.Publisher = (*Nostr)(nil)

type Nostr struct {
	bot  *nostr.Relay
	opts *config.Options
}

// New returns a Nostr client.
func New(_ *http.Client, opts *config.Options) *Nostr {
	if !opts.PublishToNostr() {
		logger.Debug("Missing required environment variable, abort.")
		return nil
	}
	// new bot for publish is needed.
	bot := &nostr.Relay{}

	return &Nostr{bot: bot, opts: opts}
}

// Publish publish text to the Nostr of given cols and args.
// A context should contain a `reduxer.Reduxer` via `publish.PubBundle` struct.
func (n *Nostr) Publish(ctx context.Context, rdx reduxer.Reduxer, cols []wayback.Collect, args ...string) error {
	metrics.IncrementPublish(metrics.PublishNostr, metrics.StatusRequest)

	if len(cols) == 0 {
		metrics.IncrementPublish(metrics.PublishNostr, metrics.StatusFailure)
		return errors.New("publish to nostr: collects empty")
	}

	_, err := publish.Artifact(ctx, rdx, cols)
	if err != nil {
		logger.Warn("extract data failed: %v", err)
	}

	body := render.ForPublish(&render.Nostr{Cols: cols, Data: rdx}).String()
	if err = n.publish(ctx, strings.TrimSpace(body)); err != nil {
		metrics.IncrementPublish(metrics.PublishNostr, metrics.StatusFailure)
		return errors.New("publish to nostr failed: %v", err)
	}
	metrics.IncrementPublish(metrics.PublishNostr, metrics.StatusSuccess)
	return nil
}

func (n *Nostr) publish(ctx context.Context, note string) error {
	if !n.opts.PublishToNostr() {
		return fmt.Errorf("publish to nostr abort")
	}

	if note == "" {
		return fmt.Errorf("nostr validation failed: note can't be blank")
	}
	logger.Debug("send to nostr, note:\n%s", note)

	sk := n.opts.NostrPrivateKey()
	if strings.HasPrefix(sk, "nsec") {
		if _, s, e := nip19.Decode(sk); e == nil {
			sk = s.(string)
		} else {
			return fmt.Errorf("decode private key failed: %v", e)
		}
	}
	pk, err := nostr.GetPublicKey(sk)
	if err != nil {
		return fmt.Errorf("failed to get public key: %v", err)
	}
	ev := nostr.Event{
		Kind:      1,
		Content:   note,
		CreatedAt: time.Now(),
		PubKey:    pk,
		// Tags:      nostr.Tags{[]string{"foo", "bar"}},
	}
	if err := ev.Sign(sk); err != nil {
		return fmt.Errorf("calling sign err: %v", err)
	}

	g, ctx := errgroup.WithContext(ctx)
	for _, relay := range n.opts.NostrRelayURL() {
		logger.Debug(`publish note to relay: %s`, relay)
		relay := relay
		g.Go(func() error {
			defer func() {
				// recover from upstream panic
				if r := recover(); r != nil {
					logger.Error("publish to %s failed: %v", relay, r)
				}
			}()
			client := relayConnect(ctx, relay)
			if client.Connection == nil {
				return fmt.Errorf("publish to %s failed: %v", relay, <-client.ConnectionError)
			}
			// send the text note
			status := client.Publish(ctx, ev)
			if status != nostr.PublishStatusSucceeded {
				return fmt.Errorf("published to %s status is %s, not %s", relay, status, nostr.PublishStatusSucceeded)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	return nil
}

func relayConnect(ctx context.Context, url string) *nostr.Relay {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	relay, err := nostr.RelayConnect(ctx, url)
	if err != nil {
		logger.Error("Connect to Nostr relay server got unpredictable error: %v", err)
	}
	return relay
}