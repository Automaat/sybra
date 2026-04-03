#import <AppKit/AppKit.h>
#import <Carbon/Carbon.h>
#import <WebKit/WebKit.h>

// Forward declarations of Go callbacks.
extern void goHotkeyCallback(void);
extern void goSpotlightSubmit(const char *title, const char *projectID);

// Forward declarations.
void resizeSpotlightPanel(int height);

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

// --- Spotlight Panel with WKWebView ---

@interface SpotlightPanel : NSPanel
@end

@implementation SpotlightPanel
- (BOOL)canBecomeKeyWindow { return YES; }
- (BOOL)canBecomeMainWindow { return NO; }
@end

@interface SpotlightController : NSObject <WKScriptMessageHandler>
@property (nonatomic, strong) SpotlightPanel *panel;
@property (nonatomic, strong) WKWebView *webView;
@end

@implementation SpotlightController

- (void)userContentController:(WKUserContentController *)controller didReceiveScriptMessage:(WKScriptMessage *)message {
	(void)controller;
	if (![message.name isEqualToString:@"spotlight"]) return;

	NSDictionary *body = message.body;
	NSString *action = body[@"action"];

	if ([action isEqualToString:@"resize"]) {
		NSNumber *h = body[@"height"];
		if (h) resizeSpotlightPanel([h intValue]);
		return;
	} else if ([action isEqualToString:@"submit"]) {
		NSString *title = body[@"title"] ?: @"";
		NSString *projectID = body[@"projectId"] ?: @"";
		if ([title length] > 0) {
			goSpotlightSubmit([title UTF8String], [projectID UTF8String]);
		}
		[self dismiss];
	} else if ([action isEqualToString:@"dismiss"]) {
		[self dismiss];
	}
}

- (void)dismiss {
	if (self.panel) {
		[self.panel orderOut:nil];
	}
}

@end

static SpotlightPanel *spotlightPanel = nil;
static SpotlightController *spotlightCtrl = nil;

static NSString *spotlightHTML(void);

void showSpotlightPanel(const char *projectsJSON) {
	NSString *json = [NSString stringWithUTF8String:projectsJSON];

	dispatch_async(dispatch_get_main_queue(), ^{
		// Toggle off if visible.
		if (spotlightPanel && [spotlightPanel isVisible]) {
			[spotlightPanel orderOut:nil];
			return;
		}

		int width = 512;
		int height = 120;

		// Find screen with mouse cursor.
		NSPoint mouseLoc = [NSEvent mouseLocation];
		NSScreen *targetScreen = [NSScreen mainScreen];
		for (NSScreen *screen in [NSScreen screens]) {
			if (NSPointInRect(mouseLoc, screen.frame)) {
				targetScreen = screen;
				break;
			}
		}

		NSRect screenFrame = [targetScreen frame];
		CGFloat x = screenFrame.origin.x + (screenFrame.size.width - width) / 2;
		CGFloat y = screenFrame.origin.y + screenFrame.size.height * 0.70;
		NSRect panelFrame = NSMakeRect(x, y, width, height);

		// Create controller if needed.
		if (!spotlightCtrl) {
			spotlightCtrl = [[SpotlightController alloc] init];
		}

		// Create panel.
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
		[spotlightPanel setBackgroundColor:[NSColor clearColor]];
		[spotlightPanel setOpaque:NO];
		[spotlightPanel setHasShadow:YES];
		[spotlightPanel setHidesOnDeactivate:NO];
		[spotlightPanel setFloatingPanel:YES];

		spotlightCtrl.panel = spotlightPanel;

		// Create WKWebView.
		WKWebViewConfiguration *config = [[WKWebViewConfiguration alloc] init];
		[config.userContentController addScriptMessageHandler:spotlightCtrl name:@"spotlight"];
		// Inject project data.
		NSString *initScript = [NSString stringWithFormat:@"window.__PROJECTS__ = %@;", json];
		WKUserScript *userScript = [[WKUserScript alloc]
			initWithSource:initScript
			injectionTime:WKUserScriptInjectionTimeAtDocumentStart
			forMainFrameOnly:YES];
		[config.userContentController addUserScript:userScript];

		NSRect webFrame = NSMakeRect(0, 0, width, height);
		WKWebView *webView = [[WKWebView alloc] initWithFrame:webFrame configuration:config];
		[webView setValue:@NO forKey:@"drawsBackground"];
		webView.wantsLayer = YES;
		webView.layer.cornerRadius = 12;
		webView.layer.masksToBounds = YES;

		spotlightCtrl.webView = webView;

		spotlightPanel.contentView = webView;
		spotlightPanel.contentView.wantsLayer = YES;
		spotlightPanel.contentView.layer.cornerRadius = 12;
		spotlightPanel.contentView.layer.masksToBounds = YES;

		[webView loadHTMLString:spotlightHTML() baseURL:nil];

		[spotlightPanel orderFrontRegardless];
		[spotlightPanel makeKeyWindow];
		[spotlightPanel makeFirstResponder:webView];
	});
}

void dismissSpotlightPanel(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		if (spotlightPanel) {
			[spotlightPanel orderOut:nil];
		}
	});
}

void resizeSpotlightPanel(int height) {
	dispatch_async(dispatch_get_main_queue(), ^{
		if (!spotlightPanel || ![spotlightPanel isVisible]) return;
		NSRect frame = [spotlightPanel frame];
		CGFloat dy = height - frame.size.height;
		frame.origin.y -= dy;
		frame.size.height = height;
		[spotlightPanel setFrame:frame display:YES animate:NO];
	});
}

static NSString *spotlightHTML(void) {
	return @"<!DOCTYPE html>"
	"<html><head><meta charset='utf-8'>"
	"<style>"
	"*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }"
	"html, body { background: transparent; font-family: -apple-system, BlinkMacSystemFont, sans-serif; color: #e2e2e8; }"
	"body { border: 1px solid rgba(140,120,170,0.35); border-radius: 12px; background: rgba(45,38,65,0.97); overflow: hidden; }"
	""
	"#title-row { position: relative; }"
	"#highlight { position: absolute; inset: 0; overflow: hidden; white-space: pre; padding: 14px 20px; font-size: 16px; color: transparent; pointer-events: none; line-height: 1.4; }"
	"#highlight mark { background: rgba(139,92,246,0.3); color: transparent; border-radius: 3px; }"
	"#title { display: block; width: 100%; border: none; outline: none; background: transparent; padding: 14px 20px; font-size: 16px; color: #e2e2e8; line-height: 1.4; }"
	"#title::placeholder { color: rgba(160,150,180,0.55); }"
	""
	"#project-row { border-top: 1px solid rgba(140,120,170,0.2); padding: 8px 20px; display: none; align-items: center; gap: 8px; font-size: 12px; color: rgba(160,150,180,0.7); }"
	"#project-row.visible { display: flex; }"
	""
	".folder-icon { width: 14px; height: 14px; flex-shrink: 0; opacity: 0.5; }"
	""
	".chip { display: inline-flex; align-items: center; gap: 4px; background: rgba(139,92,246,0.15); color: rgba(180,160,220,0.9); padding: 2px 8px; border-radius: 6px; font-weight: 500; font-size: 12px; }"
	".chip button { background: none; border: none; color: inherit; cursor: pointer; padding: 0; margin-left: 2px; opacity: 0.7; font-size: 11px; }"
	".chip button:hover { opacity: 1; }"
	""
	"#project-search { display: none; border: none; outline: none; background: transparent; font-size: 12px; color: #e2e2e8; flex: 1; }"
	"#project-search::placeholder { color: rgba(160,150,180,0.55); }"
	"#project-search.visible { display: block; }"
	""
	"#dropdown { display: none; position: absolute; bottom: 100%; left: 0; width: 100%; max-height: 192px; overflow-y: auto; background: rgba(50,42,72,0.98); border: 1px solid rgba(140,120,170,0.3); border-radius: 8px; margin-bottom: 4px; }"
	"#dropdown.visible { display: block; }"
	"#dropdown button { display: flex; width: 100%; align-items: center; gap: 8px; padding: 6px 16px; border: none; background: none; color: #e2e2e8; font-size: 12px; cursor: pointer; text-align: left; }"
	"#dropdown button:hover { background: rgba(139,92,246,0.12); }"
	".no-match { padding: 6px 16px; font-size: 12px; color: rgba(160,150,180,0.5); }"
	"</style></head><body>"
	""
	"<div id='title-row'>"
	"  <div id='highlight'></div>"
	"  <input id='title' type='text' placeholder='Task title, link, or note...' autocomplete='off' spellcheck='false' />"
	"</div>"
	"<div id='project-row'>"
	"  <svg class='folder-icon' fill='none' stroke='currentColor' viewBox='0 0 24 24'><path stroke-linecap='round' stroke-linejoin='round' stroke-width='2' d='M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2h-6l-2-2H5a2 2 0 00-2 2z'/></svg>"
	"  <span id='chip-area'></span>"
	"  <span id='match-hint'></span>"
	"  <input id='project-search' type='text' placeholder='Project (optional)...' autocomplete='off' />"
	"  <div id='dropdown'></div>"
	"</div>"
	""
	"<script>"
	"const projects = window.__PROJECTS__ || [];"
	"const titleInput = document.getElementById('title');"
	"const highlight = document.getElementById('highlight');"
	"const projectRow = document.getElementById('project-row');"
	"const chipArea = document.getElementById('chip-area');"
	"const matchHint = document.getElementById('match-hint');"
	"const projectSearch = document.getElementById('project-search');"
	"const dropdown = document.getElementById('dropdown');"
	""
	"let selectedProject = null;"
	"let userOverrode = false;"
	"let dropdownOpen = false;"
	""
	"const ghRe = /(?:https?:\\/\\/)?github\\.com\\/(([^\\/\\s]+)\\/([^\\/\\s#?]+))/gi;"
	""
	"function detectProject(input) {"
	"  if (!input || projects.length === 0) return null;"
	"  for (const m of input.matchAll(ghRe)) {"
	"    const owner = m[2].toLowerCase();"
	"    const repo = m[3].replace(/\\.git$/, '').toLowerCase();"
	"    const found = projects.find(p => p.owner.toLowerCase() === owner && p.repo.toLowerCase() === repo);"
	"    if (found) {"
	"      const s = m.index + m[0].indexOf(m[1]);"
	"      return { project: found, matchType: 'url', matchStart: s, matchEnd: s + m[1].length };"
	"    }"
	"  }"
	"  const words = input.split(/[\\s,;:!?()\\[\\]{}]+/).filter(Boolean);"
	"  for (const word of words) {"
	"    const w = word.toLowerCase();"
	"    const found = projects.find(p => p.repo.toLowerCase() === w || p.name.toLowerCase() === w);"
	"    if (found) {"
	"      const idx = input.indexOf(word);"
	"      return { project: found, matchType: 'name', matchStart: idx, matchEnd: idx + word.length };"
	"    }"
	"  }"
	"  return null;"
	"}"
	""
	"function esc(s) { const d = document.createElement('span'); d.textContent = s; return d.innerHTML; }"
	""
	"function updateHighlight() {"
	"  const val = titleInput.value;"
	"  const det = detectProject(val);"
	"  if (det && !userOverrode) {"
	"    selectedProject = det.project.id;"
	"    const before = esc(val.slice(0, det.matchStart));"
	"    const match = esc(val.slice(det.matchStart, det.matchEnd));"
	"    const after = esc(val.slice(det.matchEnd));"
	"    highlight.innerHTML = before + '<mark>' + match + '</mark>' + after;"
	"  } else {"
	"    highlight.innerHTML = '';"
	"    if (!userOverrode) selectedProject = null;"
	"  }"
	"  updateProjectRow();"
	"}"
	""
	"function updateProjectRow() {"
	"  if (projects.length === 0) { projectRow.classList.remove('visible'); resize(); return; }"
	"  projectRow.classList.add('visible');"
	"  const det = detectProject(titleInput.value);"
	""
	"  if (det && !userOverrode) {"
	"    chipArea.innerHTML = '<span class=\"chip\">' + esc(det.project.owner + '/' + det.project.repo)"
	"      + ' <button onclick=\"dismissDetection()\">&times;</button></span>';"
	"    matchHint.textContent = 'from ' + (det.matchType === 'url' ? 'link' : 'title');"
	"    projectSearch.classList.remove('visible');"
	"    dropdown.classList.remove('visible');"
	"  } else if (selectedProject && userOverrode) {"
	"    const p = projects.find(pr => pr.id === selectedProject);"
	"    chipArea.innerHTML = '<span class=\"chip\">' + esc(p ? p.id : selectedProject)"
	"      + ' <button onclick=\"clearManual()\">&times;</button></span>';"
	"    matchHint.textContent = '';"
	"    projectSearch.classList.remove('visible');"
	"    dropdown.classList.remove('visible');"
	"  } else {"
	"    chipArea.innerHTML = '';"
	"    matchHint.textContent = '';"
	"    projectSearch.classList.add('visible');"
	"  }"
	"  resize();"
	"}"
	""
	"function resize() {"
	"  const h = document.body.offsetHeight;"
	"  window.webkit.messageHandlers.spotlight.postMessage({ action: 'resize', height: h });"
	"}"
	""
	"function dismissDetection() { selectedProject = null; userOverrode = true; updateHighlight(); titleInput.focus(); }"
	"function clearManual() { selectedProject = null; userOverrode = false; updateHighlight(); titleInput.focus(); }"
	""
	"function selectManual(id) { selectedProject = id; userOverrode = true; dropdownOpen = false; projectSearch.value = ''; updateProjectRow(); titleInput.focus(); }"
	""
	"function renderDropdown() {"
	"  const q = projectSearch.value.toLowerCase();"
	"  const filtered = projects.filter(p => !q || p.id.toLowerCase().includes(q) || p.name.toLowerCase().includes(q));"
	"  if (filtered.length === 0) {"
	"    dropdown.innerHTML = '<div class=\"no-match\">No matches</div>';"
	"  } else {"
	"    dropdown.innerHTML = filtered.map(p =>"
	"      '<button onmousedown=\"selectManual(\\'' + esc(p.id) + '\\')\">' +"
	"      '<svg class=\"folder-icon\" fill=\"none\" stroke=\"currentColor\" viewBox=\"0 0 24 24\"><path stroke-linecap=\"round\" stroke-linejoin=\"round\" stroke-width=\"2\" d=\"M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2h-6l-2-2H5a2 2 0 00-2 2z\"/></svg>' +"
	"      esc(p.owner + '/' + p.repo) + '</button>'"
	"    ).join('');"
	"  }"
	"  dropdown.classList.toggle('visible', dropdownOpen);"
	"  resize();"
	"}"
	""
	"titleInput.addEventListener('input', updateHighlight);"
	""
	"titleInput.addEventListener('keydown', function(e) {"
	"  if (e.key === 'Enter') {"
	"    e.preventDefault();"
	"    const title = titleInput.value.trim();"
	"    if (title) {"
	"      window.webkit.messageHandlers.spotlight.postMessage({ action: 'submit', title: title, projectId: selectedProject || '' });"
	"    }"
	"  } else if (e.key === 'Escape') {"
	"    e.preventDefault();"
	"    window.webkit.messageHandlers.spotlight.postMessage({ action: 'dismiss' });"
	"  } else if (e.key === 'Tab' && projects.length > 0) {"
	"    e.preventDefault();"
	"    projectSearch.classList.add('visible');"
	"    projectSearch.focus();"
	"  }"
	"});"
	""
	"projectSearch.addEventListener('focus', function() { dropdownOpen = true; renderDropdown(); });"
	"projectSearch.addEventListener('blur', function() { setTimeout(function() { dropdownOpen = false; dropdown.classList.remove('visible'); resize(); }, 150); });"
	"projectSearch.addEventListener('input', renderDropdown);"
	"projectSearch.addEventListener('keydown', function(e) {"
	"  if (e.key === 'Escape') { e.preventDefault(); dropdownOpen = false; dropdown.classList.remove('visible'); titleInput.focus(); resize(); }"
	"  else if (e.key === 'Enter') { e.preventDefault(); titleInput.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter' })); }"
	"});"
	""
	"titleInput.focus();"
	"updateProjectRow();"
	"setTimeout(resize, 0);"
	"</script>"
	"</body></html>";
}
