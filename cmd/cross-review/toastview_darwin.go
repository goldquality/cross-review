// Тосты: панели справа вверху экрана, как уведомления macOS, но с кнопками. Системное уведомление с
// кнопками доступно только подписанному приложению, а диалог osascript встаёт посреди экрана и
// забирает фокус. Панель не становится ключевым окном: набор текста и Enter остаются в том
// приложении, где работает участник.
//
// Код на Objective-C — в преамбуле cgo, а не в файле .m: за каждый пакет с .m Go добавляет при
// сборке -lobjc, а он уже есть у systray, и линкер предупреждал бы о дубле.
package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa -framework QuartzCore

#import <Cocoa/Cocoa.h>
#import <QuartzCore/QuartzCore.h>
// crToastDone — в toast_darwin.go. Здесь не подключается _cgo_export.h: в преамбуле файла с
// //export могут быть только объявления, а файл с определениями не видит этот заголовок.
extern void crToastDone(long long id, int action);

static const CGFloat kWidth = 360, kPad = 14, kIcon = 32, kGap = 10, kMargin = 12, kSpacing = 8,
                     kRadius = 14, kButtonH = 26;
static const NSUInteger kMaxPlain = 4; // тостов без кнопок на экране, лишние закрываются

@interface CRToastPanel : NSPanel
@end

@implementation CRToastPanel
- (BOOL)canBecomeKeyWindow { return NO; }
- (BOOL)canBecomeMainWindow { return NO; }
@end

@interface CRToast : NSObject
@property long long tid;
@property(strong) CRToastPanel *panel;
@property BOOL hasButtons;
@property BOOL closing;
@property BOOL hovered;
@end

@implementation CRToast
@end

static NSMutableArray<CRToast *> *toasts; // сверху вниз, трогается только в главном потоке

static void crClose(CRToast *t, int action, BOOL report);

// CRContent — содержимое тоста: координаты сверху вниз, клик по тосту без кнопок закрывает его,
// пока курсор над тостом, он не закрывается сам.
@interface CRContent : NSView
@property(weak) CRToast *toast;
@property(strong) id target; // цель кнопок: у кнопки ссылка на цель слабая
@end

@implementation CRContent
- (BOOL)isFlipped { return YES; }
- (BOOL)acceptsFirstMouse:(NSEvent *)e { return YES; }
- (void)mouseDown:(NSEvent *)e {}
- (void)mouseUp:(NSEvent *)e {
  CRToast *t = self.toast;
  if (t && !t.hasButtons) crClose(t, -1, YES);
}
- (void)updateTrackingAreas {
  [super updateTrackingAreas];
  for (NSTrackingArea *a in self.trackingAreas) [self removeTrackingArea:a];
  [self addTrackingArea:[[NSTrackingArea alloc]
                            initWithRect:self.bounds
                                 options:NSTrackingMouseEnteredAndExited | NSTrackingActiveAlways |
                                         NSTrackingInVisibleRect
                                   owner:self
                                userInfo:nil]];
}
- (void)mouseEntered:(NSEvent *)e { self.toast.hovered = YES; }
- (void)mouseExited:(NSEvent *)e { self.toast.hovered = NO; }
@end

// CRButton рисует фон сам: обычная кнопка в неактивном окне теряет цвет, а панель тоста ключевой не
// становится.
@interface CRButton : NSButton
@property BOOL primary;
@end

@implementation CRButton
- (BOOL)acceptsFirstMouse:(NSEvent *)e { return YES; }
- (void)paint {
  [self.effectiveAppearance performAsCurrentDrawingAppearance:^{
    NSColor *bg = self.primary ? NSColor.controlAccentColor
                               : [NSColor.labelColor colorWithAlphaComponent:0.1];
    self.layer.backgroundColor = bg.CGColor;
  }];
}
- (void)viewDidChangeEffectiveAppearance {
  [super viewDidChangeEffectiveAppearance];
  [self paint];
}
- (void)mouseDown:(NSEvent *)e {
  self.alphaValue = 0.7;
  [super mouseDown:e]; // следит за кнопкой мыши до отпускания и шлёт action
  self.alphaValue = 1;
}
@end

@interface CRButtonTarget : NSObject
@property(weak) CRToast *toast;
@end

@implementation CRButtonTarget
- (void)clicked:(NSButton *)b {
  CRToast *t = self.toast;
  if (t && !t.closing) crToastDone(t.tid, (int)b.tag);
}
@end

static NSScreen *crScreen(void) { return NSScreen.mainScreen ?: NSScreen.screens.firstObject; }

// crLayout ставит тосты столбиком от правого верхнего угла; вызывать внутри группы анимации, чтобы
// сдвиги были плавными.
static void crLayout(void) {
  NSRect vf = crScreen().visibleFrame;
  CGFloat top = NSMaxY(vf) - kMargin;
  for (CRToast *t in toasts) {
    if (t.closing) continue;
    NSRect f = t.panel.frame;
    f.origin.x = NSMaxX(vf) - kMargin - f.size.width;
    f.origin.y = top - f.size.height;
    top = f.origin.y - kSpacing;
    [t.panel.animator setFrame:f display:YES];
  }
}

static void crClose(CRToast *t, int action, BOOL report) {
  if (t.closing) return;
  t.closing = YES;
  if (report) crToastDone(t.tid, action);
  [NSAnimationContext
      runAnimationGroup:^(NSAnimationContext *ctx) {
        ctx.duration = 0.2;
        t.panel.animator.alphaValue = 0;
        crLayout();
      }
      completionHandler:^{
        [t.panel orderOut:nil];
        [toasts removeObject:t];
      }];
}

static void crAutoClose(CRToast *t, double seconds) {
  dispatch_after(dispatch_time(DISPATCH_TIME_NOW, (int64_t)(seconds * NSEC_PER_SEC)),
                 dispatch_get_main_queue(), ^{
                   if (t.closing) return;
                   if (t.hovered) {
                     crAutoClose(t, 1);
                     return;
                   }
                   crClose(t, -1, YES);
                 });
}

static NSImage *crMask(CGFloat r) {
  NSImage *img = [NSImage imageWithSize:NSMakeSize(2 * r + 1, 2 * r + 1)
                                flipped:NO
                         drawingHandler:^BOOL(NSRect rect) {
                           [NSColor.blackColor set];
                           [[NSBezierPath bezierPathWithRoundedRect:rect xRadius:r yRadius:r] fill];
                           return YES;
                         }];
  img.capInsets = NSEdgeInsetsMake(r, r, r, r);
  img.resizingMode = NSImageResizingModeStretch;
  return img;
}

static NSTextField *crLabel(NSString *s, NSFont *font, NSColor *color, NSInteger lines, CGFloat w) {
  NSTextField *l = [NSTextField wrappingLabelWithString:s];
  l.font = font;
  l.textColor = color;
  l.maximumNumberOfLines = lines;
  l.lineBreakMode = NSLineBreakByWordWrapping;
  l.cell.truncatesLastVisibleLine = YES;
  l.selectable = NO;
  l.preferredMaxLayoutWidth = w;
  NSSize sz = [l.cell cellSizeForBounds:NSMakeRect(0, 0, w, CGFLOAT_MAX)];
  l.frame = NSMakeRect(0, 0, w, ceil(sz.height));
  return l;
}

static void crShow(long long tid, NSString *title, NSString *body, NSString *footer,
                   NSArray<NSString *> *buttons, int primary, double seconds, BOOL sound,
                   NSData *icon) {
  if (!toasts) toasts = [NSMutableArray array];
  CRToast *t = [CRToast new];
  t.tid = tid;
  t.hasButtons = buttons.count > 0;

  CGFloat textX = kPad + kIcon + kGap, textW = kWidth - textX - kPad;
  CRContent *content = [[CRContent alloc] initWithFrame:NSZeroRect];
  content.toast = t;
  CGFloat y = kPad - 1;

  NSTextField *tl = crLabel(title, [NSFont boldSystemFontOfSize:13], NSColor.labelColor, 1, textW);
  tl.frame = NSMakeRect(textX, y, textW, NSHeight(tl.frame));
  [content addSubview:tl];
  y = NSMaxY(tl.frame) + 2;
  if (body.length) {
    NSTextField *bl = crLabel(body, [NSFont systemFontOfSize:12.5], NSColor.labelColor, 6, textW);
    bl.frame = NSMakeRect(textX, y, textW, NSHeight(bl.frame));
    [content addSubview:bl];
    y = NSMaxY(bl.frame);
  }
  if (footer.length) {
    NSTextField *fl =
        crLabel(footer, [NSFont systemFontOfSize:11], NSColor.secondaryLabelColor, 2, textW);
    fl.frame = NSMakeRect(textX, y + 4, textW, NSHeight(fl.frame));
    [content addSubview:fl];
    y = NSMaxY(fl.frame);
  }
  if (t.hasButtons) {
    CRButtonTarget *target = [CRButtonTarget new];
    target.toast = t;
    content.target = target;
    y += 12;
    CGFloat x = kWidth - kPad;
    for (NSInteger i = buttons.count - 1; i >= 0; i--) {
      CRButton *b = [CRButton new];
      b.primary = (i == primary);
      b.bordered = NO;
      b.wantsLayer = YES;
      b.layer.cornerRadius = 7;
      b.focusRingType = NSFocusRingTypeNone;
      b.tag = i;
      b.target = target;
      b.action = @selector(clicked:);
      NSFont *f = [NSFont systemFontOfSize:12 weight:NSFontWeightMedium];
      NSColor *fg = b.primary ? NSColor.whiteColor : NSColor.labelColor;
      b.attributedTitle = [[NSAttributedString alloc]
          initWithString:buttons[i]
              attributes:@{NSFontAttributeName : f, NSForegroundColorAttributeName : fg}];
      CGFloat w = ceil([buttons[i] sizeWithAttributes:@{NSFontAttributeName : f}].width) + 26;
      x -= w;
      b.frame = NSMakeRect(x, y, w, kButtonH);
      x -= 8;
      [b paint];
      [content addSubview:b];
    }
    y += kButtonH;
  }
  y += kPad;
  CGFloat h = MAX(y, kPad * 2 + kIcon);

  NSImageView *iv = [NSImageView imageViewWithImage:[[NSImage alloc] initWithData:icon]];
  iv.imageScaling = NSImageScaleProportionallyUpOrDown;
  iv.frame = NSMakeRect(kPad, kPad, kIcon, kIcon);
  [content addSubview:iv];

  NSView *bar = nil;
  if (t.hasButtons && seconds > 0) {
    NSView *track = [[NSView alloc] initWithFrame:NSMakeRect(kRadius, h - 6, kWidth - 2 * kRadius, 3)];
    track.wantsLayer = YES;
    track.layer.cornerRadius = 1.5;
    bar = [[NSView alloc] initWithFrame:track.frame];
    bar.wantsLayer = YES;
    bar.layer.cornerRadius = 1.5;
    [content.effectiveAppearance performAsCurrentDrawingAppearance:^{
      track.layer.backgroundColor = [NSColor.labelColor colorWithAlphaComponent:0.08].CGColor;
      bar.layer.backgroundColor = NSColor.controlAccentColor.CGColor;
    }];
    [content addSubview:track];
    [content addSubview:bar];
  }

  content.frame = NSMakeRect(0, 0, kWidth, h);
  NSVisualEffectView *fx = [[NSVisualEffectView alloc] initWithFrame:content.frame];
  fx.material = NSVisualEffectMaterialPopover;
  fx.blendingMode = NSVisualEffectBlendingModeBehindWindow;
  fx.state = NSVisualEffectStateActive;
  fx.maskImage = crMask(kRadius);
  [fx addSubview:content];

  CRToastPanel *p = [[CRToastPanel alloc]
      initWithContentRect:content.frame
                styleMask:NSWindowStyleMaskBorderless | NSWindowStyleMaskNonactivatingPanel
                  backing:NSBackingStoreBuffered
                    defer:NO];
  p.contentView = fx;
  p.opaque = NO;
  p.backgroundColor = NSColor.clearColor;
  p.hasShadow = YES;
  p.level = NSStatusWindowLevel;
  p.hidesOnDeactivate = NO;
  p.releasedWhenClosed = NO;
  p.floatingPanel = YES;
  p.becomesKeyOnlyIfNeeded = YES;
  p.animationBehavior = NSWindowAnimationBehaviorNone;
  p.collectionBehavior = NSWindowCollectionBehaviorCanJoinAllSpaces |
                         NSWindowCollectionBehaviorStationary |
                         NSWindowCollectionBehaviorFullScreenAuxiliary |
                         NSWindowCollectionBehaviorIgnoresCycle;
  t.panel = p;

  // Новый тост — сверху, выезжает справа; остальные съезжают вниз.
  NSRect vf = crScreen().visibleFrame;
  [p setFrame:NSMakeRect(NSMaxX(vf) - kMargin - kWidth + 40, NSMaxY(vf) - kMargin - h, kWidth, h)
      display:NO];
  p.alphaValue = 0;
  [toasts insertObject:t atIndex:0];
  NSUInteger plain = 0;
  for (CRToast *o in [toasts copy]) {
    if (!o.hasButtons && !o.closing && ++plain > kMaxPlain) crClose(o, -1, YES);
  }
  [p orderFrontRegardless];
  [NSAnimationContext runAnimationGroup:^(NSAnimationContext *ctx) {
    ctx.duration = 0.25;
    ctx.timingFunction = [CAMediaTimingFunction functionWithName:kCAMediaTimingFunctionEaseOut];
    p.animator.alphaValue = 1;
    crLayout();
  }];
  [p invalidateShadow];

  if (bar) {
    NSRect end = bar.frame;
    end.size.width = 0;
    [NSAnimationContext runAnimationGroup:^(NSAnimationContext *ctx) {
      ctx.duration = seconds;
      ctx.timingFunction = [CAMediaTimingFunction functionWithName:kCAMediaTimingFunctionLinear];
      bar.animator.frame = end;
    }];
  }
  if (!t.hasButtons) crAutoClose(t, seconds > 0 ? seconds : 6);
  if (sound) [[NSSound soundNamed:@"Glass"] play];
}

void crToastShow(long long id, const char *title, const char *body, const char *footer,
                 const char *buttons, int primary, double seconds, int sound, const void *icon,
                 int iconLen) {
  NSString *t = [NSString stringWithUTF8String:title] ?: @"";
  NSString *b = [NSString stringWithUTF8String:body] ?: @"";
  NSString *f = [NSString stringWithUTF8String:footer] ?: @"";
  NSString *bs = [NSString stringWithUTF8String:buttons] ?: @"";
  NSArray<NSString *> *list = bs.length ? [bs componentsSeparatedByString:@"\n"] : @[];
  NSData *img = [NSData dataWithBytes:icon length:iconLen];
  dispatch_async(dispatch_get_main_queue(), ^{
    crShow(id, t, b, f, list, primary, seconds, sound != 0, img);
  });
}

void crToastClose(long long id) {
  dispatch_async(dispatch_get_main_queue(), ^{
    for (CRToast *t in toasts) {
      if (t.tid == id) {
        crClose(t, 0, NO);
        return;
      }
    }
  });
}
*/
import "C"
