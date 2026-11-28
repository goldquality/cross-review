import { Call } from "/wails/runtime.js";
import { el, fill, text } from "/dom.js";

const call = (method, ...args) => Call.ByName("main.UI." + method, ...args);
const id = Number(new URLSearchParams(location.search).get("id"));

async function main() {
  const s = await call("Toast", id);
  text("title", s.Title);
  text("body", s.Body);
  text("footer", s.Footer);
  const buttons = (s.Buttons || []).map((label, i) => {
    const b = el("button", i === s.Primary ? "primary" : "ghost", label);
    b.onclick = (e) => { e.stopPropagation(); call("ToastAction", id, i); };
    return b;
  });
  fill(document.getElementById("buttons"), buttons);
  if (!buttons.length) {
    document.body.classList.add("clickable");
    document.body.onclick = () => call("ToastAction", id, -1);
  } else if (s.Seconds > 0) {
    const bar = document.getElementById("bar");
    bar.hidden = false;
    bar.firstElementChild.style.animationDuration = s.Seconds + "s";
  }
  call("RenderCheck", document.querySelectorAll("img").length);
  // Окно подстраивается под текст: Go ставит высоту и раскладывает столбик. Перемеряем при каждом
  // изменении размера: пока окно скрыто, у WebView бывает нулевая ширина (GTK), и первая мерка врёт.
  // requestAnimationFrame не годится: в скрытом окне кадры не рисуются.
  const card = document.getElementById("card");
  let last = 0;
  const measure = () => {
    const h = Math.ceil(card.getBoundingClientRect().height + (buttons.length && s.Seconds > 0 ? 3 : 0));
    if (h !== last && document.documentElement.clientWidth > 0) {
      last = h;
      call("ToastResize", id, h);
    }
  };
  new ResizeObserver(measure).observe(card);
  measure();
}

main();
