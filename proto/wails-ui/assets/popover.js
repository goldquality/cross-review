import { Call, Events, Clipboard } from "/wails/runtime.js";
import { el, fill, text } from "/dom.js";

const call = (method, ...args) => Call.ByName("main.UI." + method, ...args);
const $ = (id) => document.getElementById(id);

async function refresh() {
  const s = await call("State");
  text("name", s.Name);
  $("dot").className = "dot " + (s.PausedUntil ? "paused" : s.Online ? "online" : "offline");
  text("balance", "баланс " + s.Balance);
  text("status", s.PausedUntil ? "пауза до " + s.PausedUntil + " — задания не приходят" : s.Online ? "онлайн, жду заданий" : "нет связи с брокером");

  $("current-box").hidden = !s.Current;
  if (s.Current) text("current", `${s.Current.Short} · просит ${s.Current.Requester} · ${s.Current.Minutes} мин`);

  fill($("requests"), (s.Requests || []).map((r) => {
    const li = el("li");
    const head = el("div", "row");
    head.append(el("span", "mono", r.Short), el("span", "muted small", r.When + " · " + r.Status));
    li.append(head);
    const jobs = (r.Jobs || []).map((j) => (j.Reviewer || "ждёт ревьюера") + ": " + j.Status).join("; ");
    li.append(el("div", "muted small", jobs));
    return li;
  }));
  if (!s.Requests || !s.Requests.length) fill($("requests"), [el("li", "muted small", "Запросов не было")]);

  const me = (m) => m.Name + (m.Me ? " (ты)" : "");
  fill($("team"), (s.Team || []).map((m) => {
    const li = el("li", "row");
    li.append(el("span", "", me(m)), el("span", "muted small", m.State));
    return li;
  }));
  fill($("top"), (s.Top || []).map((m) => {
    const li = el("li", "row");
    li.append(el("span", "", me(m)), el("span", "muted small", String(m.Balance)));
    return li;
  }));

  text("pause-label", s.PausedUntil ? "Пауза до " + s.PausedUntil : "Пауза");
  const btns = [1, 3, 6, 12, 24].map((h) => {
    const b = el("button", "ghost", h + " ч");
    b.onclick = () => call("Pause", h).then(refresh);
    return b;
  });
  if (s.PausedUntil) {
    const off = el("button", "ghost", "Снять");
    off.onclick = () => call("Pause", 0).then(refresh);
    btns.push(off);
  }
  fill($("pause"), btns);

  fill($("projects"), (s.Projects || []).map((p) => el("li", "mono", p)));
  $("settings-err").hidden = !s.SettingsErr;
  text("settings-err", s.SettingsErr ? "Ошибка в настройках: " + s.SettingsErr : "");

  // Проверка для CI: строки брокера не должны были превратиться в элементы.
  call("RenderCheck", document.querySelectorAll("img").length);
}

$("request").addEventListener("submit", async (e) => {
  e.preventDefault();
  const out = $("request-result");
  out.className = "muted small";
  try {
    out.textContent = await call("Request", $("link").value);
    $("link").value = "";
    refresh();
  } catch (err) {
    out.className = "err small";
    out.textContent = err && err.message ? err.message : String(err);
  }
});
$("paste").onclick = async () => { $("link").value = (await Clipboard.Text()).trim(); };
$("stop").onclick = () => call("StopCurrent");
$("settings").onclick = () => call("OpenSettings");

Events.On("state", refresh);
window.addEventListener("focus", refresh);
refresh();
