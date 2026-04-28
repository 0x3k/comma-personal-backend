package main

import (
	"context"
	"encoding/base64"
	"log"
	"strings"

	alprcrypto "comma-personal-backend/internal/alpr/crypto"
	"comma-personal-backend/internal/alpr/heuristic"
	"comma-personal-backend/internal/alpr/notify"
	"comma-personal-backend/internal/config"
	"comma-personal-backend/internal/db"
)

// buildALPRNotifyDispatcher wires the email + webhook senders (each
// constructed conditionally on config) into a notify.Dispatcher. The
// returned dispatcher is non-nil even when no senders are configured;
// the dispatcher's Test / Dispatch methods both no-op when SenderCount
// is zero, which is the correct behaviour for the
// "no-config no-op" acceptance criterion.
//
// Returns nil only when the supplied ALPR config is itself nil. That
// is unreachable in practice (config.LoadALPR always returns a struct)
// but the guard means cmd/server can call this in any order without
// ordering bugs.
func buildALPRNotifyDispatcher(cfg *config.ALPRConfig, queries *db.Queries, keyring *alprcrypto.Keyring) *notify.Dispatcher {
	if cfg == nil {
		return nil
	}
	senders := []notify.Sender{}
	if email := notify.NewEmailSender(notify.EmailConfig{
		Host:     cfg.NotifySMTPHost,
		Port:     cfg.NotifySMTPPort,
		Username: cfg.NotifySMTPUser,
		Password: cfg.NotifySMTPPass,
		From:     cfg.NotifySMTPFrom,
		To:       cfg.NotifyEmailTo,
		TLSMode:  cfg.NotifySMTPTLS,
	}); email != nil {
		senders = append(senders, email)
	}
	if webhook := notify.NewWebhookSender(notify.WebhookConfig{
		URL: cfg.NotifyWebhookURL,
	}); webhook != nil {
		senders = append(senders, webhook)
	}
	enricher := newAlertEnricher(queries, keyring)
	return notify.New(queries, senders, enricher, notify.Config{
		MinSeverity:  cfg.NotifyMinSeverity,
		DedupHours:   cfg.NotifyDedupHours,
		DashboardURL: cfg.DashboardURL,
	})
}

// runALPRNotifySubscriber is the long-running goroutine that translates
// heuristic.AlertCreated events into notify.Dispatcher.Dispatch calls.
// Started by startWorkers AFTER the heuristic worker so deps.alprAlertCreated
// is already constructed.
//
// Stops cleanly on ctx cancellation. A nil dispatcher or nil channel
// makes this a no-op so a deployment that has not opted into
// notifications still drains the channel from a single owner.
func runALPRNotifySubscriber(ctx context.Context, dispatcher *notify.Dispatcher, alerts <-chan heuristic.AlertCreated) {
	if dispatcher == nil || alerts == nil {
		// We still need to drain the channel if it is non-nil but the
		// dispatcher is, otherwise the heuristic's emit() blocks on a
		// full channel even though nothing reads from it.
		if alerts != nil {
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-alerts:
					if !ok {
						return
					}
				}
			}
		}
		return
	}
	log.Printf("alpr notify subscriber started (senders=%v, min_severity=%d)",
		dispatcher.SenderNames(), dispatcher.SenderCount())
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-alerts:
			if !ok {
				return
			}
			base := notify.AlertPayload{
				Severity:     ev.Severity,
				PlateHashB64: notify.EncodePlateHashB64(ev.PlateHash),
				Route:        ev.Route,
				DongleID:     ev.DongleID,
			}
			if err := dispatcher.Dispatch(ctx, base, ev.PlateHash); err != nil {
				log.Printf("alpr notify subscriber: dispatch: %v", err)
			}
		}
	}
}

// newAlertEnricher returns a notify.PayloadEnricher that fills in the
// decrypted plate text for an alert. The enricher is allowed to return
// an empty Plate field; the dispatcher will skip the alert in that case
// (a transient state -- a later evaluation will produce a new event
// once a sample detection is available).
//
// queries / keyring may both be nil; in that case the enricher is a
// pass-through. The dispatcher's empty-plate guard then drops the
// alert silently, which is the right behaviour for a deployment whose
// ALPR_ENCRYPTION_KEY is unset.
func newAlertEnricher(queries *db.Queries, keyring *alprcrypto.Keyring) notify.PayloadEnricher {
	return notify.PayloadEnricherFunc(func(ctx context.Context, base notify.AlertPayload) (notify.AlertPayload, error) {
		if queries == nil || keyring == nil {
			return base, nil
		}
		plateHash, err := decodePlateHashB64Local(base.PlateHashB64)
		if err != nil || len(plateHash) == 0 {
			return base, nil
		}
		encs, err := queries.ListEncountersForPlate(ctx, plateHash)
		if err != nil {
			return base, nil
		}
		// Decrypt plate text by walking the encounters and trying each
		// detection's ciphertext. Mirrors decryptPlateFromEncounters in
		// internal/api/alpr_encounters.go.
		for _, e := range encs {
			dets, err := queries.ListDetectionsForRoute(ctx, db.ListDetectionsForRouteParams{
				DongleID: e.DongleID,
				Route:    e.Route,
			})
			if err != nil {
				continue
			}
			for _, d := range dets {
				if string(d.PlateHash) != string(e.PlateHash) {
					continue
				}
				if len(d.PlateCiphertext) == 0 {
					continue
				}
				if plaintext, err := keyring.Decrypt(d.PlateCiphertext); err == nil {
					base.Plate = plaintext
					break
				}
			}
			if base.Plate != "" {
				break
			}
		}
		return base, nil
	})
}

// decodePlateHashB64Local decodes the base64-URL-no-padding form used
// by the rest of the ALPR API. Mirrors api.decodePlateHashB64 without
// the import dependency. The synthetic test endpoint uses a non-base64
// placeholder so we tolerate decode failures by returning an empty
// slice (the enricher then short-circuits to a pass-through).
func decodePlateHashB64Local(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}
