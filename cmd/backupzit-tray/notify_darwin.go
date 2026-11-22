package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework UserNotifications

#import <Foundation/Foundation.h>
#import <UserNotifications/UserNotifications.h>

// UNUserNotificationCenter raises an exception in a program that is not an
// app bundle (e.g. run from a terminal while developing): then nothing can
// be checked or asked.
static int bzHasBundle(void) {
	return [[NSBundle mainBundle] bundleIdentifier] != nil;
}

// bzNotifyAuthorization returns 0 unknown, 1 not determined, 2 denied,
// 3 allowed (also provisional and ephemeral).
static int bzNotifyAuthorization(void) {
	if (!bzHasBundle()) {
		return 0;
	}
	__block int code = 0;
	dispatch_semaphore_t done = dispatch_semaphore_create(0);
	[[UNUserNotificationCenter currentNotificationCenter]
		getNotificationSettingsWithCompletionHandler:^(UNNotificationSettings *s) {
			switch (s.authorizationStatus) {
			case UNAuthorizationStatusNotDetermined:
				code = 1;
				break;
			case UNAuthorizationStatusDenied:
				code = 2;
				break;
			default:
				code = 3;
			}
			dispatch_semaphore_signal(done);
		}];
	if (dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, 5 * NSEC_PER_SEC)) != 0) {
		return 0;
	}
	return code;
}

// bzRequestNotifications shows macOS's own "allow notifications" prompt;
// the answer is read back by the next bzNotifyAuthorization.
static void bzRequestNotifications(void) {
	if (!bzHasBundle()) {
		return;
	}
	[[UNUserNotificationCenter currentNotificationCenter]
		requestAuthorizationWithOptions:(UNAuthorizationOptionAlert | UNAuthorizationOptionSound)
		completionHandler:^(BOOL granted, NSError *err) {}];
}
*/
import "C"

// checkNotifications reports whether the menu bar app may show
// notifications (only an app bundle can have the permission).
func checkNotifications() permState {
	return notifyState(int(C.bzNotifyAuthorization()))
}

// requestNotificationPermission makes macOS ask the user.
func requestNotificationPermission() {
	C.bzRequestNotifications()
}
