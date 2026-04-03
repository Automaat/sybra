#import <AppKit/AppKit.h>
#import <Carbon/Carbon.h>

// Forward declarations of Go callbacks.
extern void goHotkeyCallback(void);
extern void goSpotlightSubmit(char *text);

// --- Hotkey ---

static OSStatus hotkeyHandler(EventHandlerCallRef next, EventRef event, void *userData) {
	(void)next; (void)event; (void)userData;
	goHotkeyCallback();
	return noErr;
}

int registerGlobalHotkey(void) {
	EventTypeSpec eventType = {kEventClassKeyboard, kEventHotKeyPressed};
	InstallApplicationEventHandler(&hotkeyHandler, 1, &eventType, NULL, NULL);

	EventHotKeyRef hotKeyRef;
	EventHotKeyID hotKeyID = {.signature = 'SYNP', .id = 1};
	OSStatus status = RegisterEventHotKey(
		kVK_Space, controlKey, hotKeyID,
		GetApplicationEventTarget(), 0, &hotKeyRef
	);
	return (int)status;
}

// --- Spotlight Panel ---

// Subclass so borderless nonactivating panel can still become key and receive keyboard.
@interface SpotlightPanel : NSPanel
@end

@implementation SpotlightPanel
- (BOOL)canBecomeKeyWindow { return YES; }
- (BOOL)canBecomeMainWindow { return NO; }
@end

// Text field delegate: Enter submits, Escape dismisses.
@interface SpotlightFieldDelegate : NSObject <NSTextFieldDelegate>
@property (nonatomic, assign) SpotlightPanel *panel;
@end

@implementation SpotlightFieldDelegate

- (BOOL)control:(NSControl *)control textView:(NSTextView *)textView doCommandBySelector:(SEL)commandSelector {
	(void)textView;
	if (commandSelector == @selector(insertNewline:)) {
		NSString *text = [control stringValue];
		if ([text length] > 0) {
			goSpotlightSubmit((char *)[text UTF8String]);
		}
		[self dismissPanel];
		return YES;
	}
	if (commandSelector == @selector(cancelOperation:)) {
		[self dismissPanel];
		return YES;
	}
	return NO;
}

- (void)dismissPanel {
	if (self.panel) {
		[self.panel orderOut:nil];
		self.panel = nil;
	}
}

@end

static SpotlightPanel *spotlightPanel = nil;
static SpotlightFieldDelegate *fieldDelegate = nil;
static char previousBundleID[256] = {0};

void showSpotlightPanel(int width, int height) {
	dispatch_async(dispatch_get_main_queue(), ^{
		// Remember the currently focused app.
		NSRunningApplication *frontApp = [[NSWorkspace sharedWorkspace] frontmostApplication];
		if (frontApp && [frontApp bundleIdentifier]) {
			strlcpy(previousBundleID, [[frontApp bundleIdentifier] UTF8String], sizeof(previousBundleID));
		} else {
			previousBundleID[0] = '\0';
		}

		// If panel already visible, dismiss it (toggle).
		if (spotlightPanel && [spotlightPanel isVisible]) {
			[spotlightPanel orderOut:nil];
			spotlightPanel = nil;
			return;
		}

		// Find screen with mouse cursor.
		NSPoint mouseLoc = [NSEvent mouseLocation];
		NSScreen *targetScreen = [NSScreen mainScreen];
		for (NSScreen *screen in [NSScreen screens]) {
			if (NSPointInRect(mouseLoc, screen.frame)) {
				targetScreen = screen;
				break;
			}
		}

		// Position: centered horizontally, near top of screen.
		NSRect screenFrame = [targetScreen frame];
		CGFloat x = screenFrame.origin.x + (screenFrame.size.width - width) / 2;
		CGFloat y = screenFrame.origin.y + screenFrame.size.height * 0.70;
		NSRect panelFrame = NSMakeRect(x, y, width, height);

		// Create nonactivating panel — receives keyboard WITHOUT activating the app.
		// NSWindowStyleMaskNonactivatingPanel MUST be set at init time.
		spotlightPanel = [[SpotlightPanel alloc]
			initWithContentRect:panelFrame
			styleMask:NSWindowStyleMaskBorderless | NSWindowStyleMaskNonactivatingPanel
			backing:NSBackingStoreBuffered
			defer:NO];

		[spotlightPanel setCollectionBehavior:
			NSWindowCollectionBehaviorCanJoinAllSpaces |
			NSWindowCollectionBehaviorFullScreenAuxiliary |
			NSWindowCollectionBehaviorStationary |
			NSWindowCollectionBehaviorIgnoresCycle];
		[spotlightPanel setLevel:kCGScreenSaverWindowLevel - 1];
		[spotlightPanel setBackgroundColor:[NSColor colorWithRed:0.12 green:0.14 blue:0.18 alpha:0.95]];
		[spotlightPanel setOpaque:NO];
		[spotlightPanel setHasShadow:YES];
		[spotlightPanel setHidesOnDeactivate:NO];
		[spotlightPanel setFloatingPanel:YES];

		// Round corners.
		[spotlightPanel.contentView setWantsLayer:YES];
		spotlightPanel.contentView.layer.cornerRadius = 12;
		spotlightPanel.contentView.layer.masksToBounds = YES;

		// Create text field.
		NSTextField *field = [[NSTextField alloc] initWithFrame:NSMakeRect(16, 16, width - 32, height - 32)];
		[field setFont:[NSFont systemFontOfSize:20 weight:NSFontWeightRegular]];
		[field setTextColor:[NSColor whiteColor]];
		[field setBackgroundColor:[NSColor clearColor]];
		[field setFocusRingType:NSFocusRingTypeNone];
		[field setBordered:NO];
		[field setBezeled:NO];
		[[field cell] setPlaceholderAttributedString:
			[[NSAttributedString alloc]
				initWithString:@"New task..."
				attributes:@{
					NSForegroundColorAttributeName: [NSColor colorWithWhite:0.5 alpha:1.0],
					NSFontAttributeName: [NSFont systemFontOfSize:20 weight:NSFontWeightRegular]
				}]];

		// Set delegate for Enter/Escape handling.
		if (!fieldDelegate) {
			fieldDelegate = [[SpotlightFieldDelegate alloc] init];
		}
		fieldDelegate.panel = spotlightPanel;
		[field setDelegate:fieldDelegate];

		[[spotlightPanel contentView] addSubview:field];

		// Show panel and focus field. NO app activation — nonactivating panel handles keyboard.
		[spotlightPanel orderFrontRegardless];
		[spotlightPanel makeKeyWindow];
		[spotlightPanel makeFirstResponder:field];
	});
}

void dismissSpotlightPanel(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		if (spotlightPanel) {
			[spotlightPanel orderOut:nil];
			spotlightPanel = nil;
		}
	});
}
