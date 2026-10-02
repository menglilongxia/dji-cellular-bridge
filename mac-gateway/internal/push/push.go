// Package push abstracts phone wake-up notifications.
//
// Production uses APNs VoIP pushes (Apple requires the app to report a
// CallKit call within ~5 seconds of a VoIP push). ConsoleService logs
// instead, for development without Apple credentials.
package push

import (
	"context"
	"errors"
	"log"
)

// Service sends wake-up notifications to the phone.
type Service interface {
	SendVoIP(ctx context.Context, deviceToken, callerName, callerURI string) error
	SendMessage(ctx context.Context, deviceToken, from, body string) error
	SendMissedCall(ctx context.Context, deviceToken, callerName string) error
}

// ConsoleService logs pushes; used in fake/dev mode.
type ConsoleService struct{}

func (ConsoleService) SendVoIP(ctx context.Context, deviceToken, callerName, callerURI string) error {
	log.Printf("push[voip]: token=%.8s… caller=%s <%s>", deviceToken, callerName, callerURI)
	return nil
}
func (ConsoleService) SendMessage(ctx context.Context, deviceToken, from, body string) error {
	log.Printf("push[message]: token=%.8s… from=%s body=%q", deviceToken, from, body)
	return nil
}
func (ConsoleService) SendMissedCall(ctx context.Context, deviceToken, callerName string) error {
	log.Printf("push[missed]: token=%.8s… caller=%s", deviceToken, callerName)
	return nil
}

// APNSService is the real Apple Push Notification service.
// NOT IMPLEMENTED YET — needs an Apple Developer account, a VoIP push
// certificate/key (.p8), and the app's push token (see docs/PROTOCOL.md).
type APNSService struct {
	KeyID  string
	TeamID string
	Bundle string // app bundle id
}

var errAPNSNotConfigured = errors.New("push: APNS not configured (need Apple Developer key + device token)")

func (a *APNSService) SendVoIP(ctx context.Context, deviceToken, callerName, callerURI string) error {
	return errAPNSNotConfigured
}
func (a *APNSService) SendMessage(ctx context.Context, deviceToken, from, body string) error {
	return errAPNSNotConfigured
}
func (a *APNSService) SendMissedCall(ctx context.Context, deviceToken, callerName string) error {
	return errAPNSNotConfigured
}
