import { Call, Events } from "/wails/runtime.js";
import { el, fill } from "/dom.js";

const call = (method, ...args) => Call.ByName("main.UI." + method, ...args);
const stack = document.getElementById("stack");

function card(t) {
  const c = el("div", "card");
  c.onmouseenter = () => call("ToastHover", t.ID, true);
  c.onmouseleave = () => call("ToastHover", t.ID, false);
  const row = el("div", "row top");
  row.append(el("div", "icon"));
  const body = el("div", "text");
  body.append(el("div", "title", t.Title));
  if (t.Body) body.append(el("div", "body", t.Body));
  if (t.Footer) body.append(el("div", "footer muted small", t.Footer));
  const buttons = (t.Buttons || []).map((label, i) => {
    const b = el("button", i === t.Primary ? "primary" : "", label);
    b.onclick = (e) => { e.stopPropagation(); call("ToastAction", t.ID, i); };
    return b;
  });
  if (buttons.length) {
    const box = el("div", "buttons");
    box.append(...buttons);
    body.append(box);
  } else {
    c.classList.add("clickable");
    c.onclick = () => call("ToastAction", t.ID, -1);
  }
  row.append(body);
  c.append(row);
  if (t.Total > 0) {
    // Полоска отсчёта: отрицательная задержка — сколько уже прошло, так она не начинается заново,
    // когда стопку перерисовывают.
    const bar = el("div", "bar");
    const fillBar = el("div");
    fillBar.style.animationDuration = t.Total + "ms";
    fillBar.style.animationDelay = -(t.Total - t.Left) + "ms";
    bar.append(fillBar);
    c.append(bar);
  }
  return c;
}

let last = 0;
function measure() {
  const h = Math.ceil(stack.getBoundingClientRect().height);
  if (h !== last && document.documentElement.clientWidth > 0) {
    last = h;
    call("PanelResize", h);
  }
}

async function refresh() {
  const list = await call("Toasts");
  fill(stack, (list || []).map(card));
  last = 0; // показать панель заново, даже если высота та же
  measure();
}

// Перемеряем при каждом изменении размера: пока окно скрыто, у WebView бывает нулевая ширина (GTK),
// и первая мерка врёт. requestAnimationFrame не годится: в скрытом окне кадры не рисуются.
new ResizeObserver(measure).observe(stack);
Events.On("toasts", refresh);
refresh();
