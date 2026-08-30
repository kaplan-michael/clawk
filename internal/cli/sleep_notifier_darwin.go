//go:build darwin

package cli

import (
	"context"
	"log"

	sleepnotifier "github.com/prashantgupta24/mac-sleep-notifier/notifier"
)

// runSleepNotifierBridge subscribes to mac-sleep-notifier and fires
// triggerTimeSync on Awake events. Separate goroutine so a hung
// notifier (the standby case) doesn't block the interval ticker.
// We keep the interval ticker as a fallback for cases where the
// notifier silently drops events (long-standby).
func runSleepNotifierBridge(ctx context.Context, logger *log.Logger) {
	notifier := sleepnotifier.GetInstance()
	ch := notifier.Start()
	defer notifier.Quit()
	for {
		select {
		case <-ctx.Done():
			return
		case activity, ok := <-ch:
			if !ok {
				return
			}
			if activity.Type == sleepnotifier.Awake {
				logger.Printf("time-sync: Awake notification received; triggering immediate push")
				triggerTimeSync()
			}
		}
	}
}
