package aurora_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/aurora"
	"github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
)

func TestNewStripeProviderRequiresBothSecrets(t *testing.T) {
	if aurora.NewStripeProvider("", "whsec") != nil {
		t.Fatal("provider with empty secret key should be nil")
	}
	if aurora.NewStripeProvider("sk_test", "") != nil {
		t.Fatal("provider with empty webhook secret should be nil")
	}
	if aurora.NewStripeProvider("sk_test", "whsec") == nil {
		t.Fatal("provider with both secrets should exist")
	}
}

func TestConstructEventRejectsBadSignature(t *testing.T) {
	p := aurora.NewStripeProvider("sk_test", "whsec_test")
	if _, err := p.ConstructEvent([]byte(`{"type":"checkout.session.completed"}`), "t=1,v1=deadbeef"); err == nil {
		t.Fatal("ConstructEvent with invalid signature should fail")
	}
}

// A body signed with a different secret must not verify: the webhook endpoint
// is public, so the signature is the only thing separating a real Stripe event
// from an anonymous request that grants itself credits.
func TestConstructEventRejectsSignatureFromAnotherSecret(t *testing.T) {
	payload := []byte(`{"id":"evt_other","type":"checkout.session.completed","data":{"object":{}}}`)
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload: payload,
		Secret:  "whsec_attacker",
	})

	p := aurora.NewStripeProvider("sk_test", "whsec_test")
	if _, err := p.ConstructEvent(payload, signed.Header); err == nil {
		t.Fatal("ConstructEvent with a signature from another secret should fail")
	}
}

// The round trip is what proves the provider hands the handler the event's
// identity, ordering timestamp, and raw object. The handler uses those fields
// for idempotency, stale-event rejection, and object decoding respectively.
func TestConstructEventRoundTrip(t *testing.T) {
	// A real Stripe body carries `object: "event"` and the API version of the
	// endpoint that sent it; stripe-go rejects a body without them before the
	// handler ever sees it, so the fixture has to look like the real thing.
	payload := []byte(`{"id":"evt_1","object":"event","api_version":"` + stripe.APIVersion + `","created":1720000000,"type":"checkout.session.completed","data":{"object":{"id":"cs_1","mode":"payment"}}}`)
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{
		Payload:   payload,
		Secret:    "whsec_test",
		Timestamp: time.Now(),
	})

	p := aurora.NewStripeProvider("sk_test", "whsec_test")
	event, err := p.ConstructEvent(payload, signed.Header)
	if err != nil {
		t.Fatalf("ConstructEvent: %v", err)
	}
	if event.ID != "evt_1" {
		t.Fatalf("event id = %q, want evt_1", event.ID)
	}
	if event.Type != "checkout.session.completed" {
		t.Fatalf("event type = %q, want checkout.session.completed", event.Type)
	}
	if event.Created != 1720000000 {
		t.Fatalf("event created = %d, want 1720000000", event.Created)
	}
	var object struct {
		ID   string `json:"id"`
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(event.Raw, &object); err != nil {
		t.Fatalf("event raw is not the object JSON: %v", err)
	}
	if object.ID != "cs_1" || object.Mode != "payment" {
		t.Fatalf("event raw object = %+v, want the checkout session", object)
	}
}
