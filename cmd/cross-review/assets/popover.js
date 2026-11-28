import { Call, Events } from "/wails/runtime.js";
import { el, fill, text, errorText } from "/dom.js";

const call = (method, ...args) => Call.ByName("main.UI." + method, ...args);
const $ = (id) => document.getElementById(id);
let view = null;

function minutes(startedMs) {
  return Math.max(0, Math.floor((Date.now() - startedMs) / 60000));
}

function member(m, extra) {
  const li = el("li", "row");
  li.append(el("span", m.Me ? "me" : "", m.Name + (m.Me ? " (ты)" : "")), el("span", "muted small", extra));
  return li;
}

function render(v) {
  view = v;
  text("name", v.Name);
  $("dot").className = "dot " + v.Kind;
  text("balance", "баланс " + v.Balance);
  text("status", v.Status);

  $("update").hidden = !v.Update;
  text("update-text", v.Update ? "Вышла " + v.Update : "");
  $("settings-err").hidden = !v.SettingsErr;
  text("settings-err-text", v.SettingsErr ? "Ошибка в настройках: " + v.SettingsErr : "");

  $("current-box").hidden = !v.Current;
  if (v.Current) text("current", `${v.Current.Short} · просит ${v.Current.Requester} · ${minutes(v.Current.Started)} мин`);
  $("last-log").hidden = !v.LastLog || !!v.Current;

  $("team-box").hidden = !(v.Team && v.Team.length) && !(v.Top && v.Top.length);
  text("team-label", "Онлайн: " + ((v.Team && v.Team.length) || 0));
  fill($("team"), (v.Team || []).map((m) => member(m, m.State)));
  fill($("top"), (v.Top || []).map((m) => member(m, String(m.Balance))));

  text("pause-label", v.PausedUntil ? "Пауза до " + v.PausedUntil : "Пауза");
  const pause = (v.PauseHours || []).map((h) => {
    const b = el("button", "", h + " ч");
    b.onclick = () => act(() => call("Pause", h));
    return b;
  });
  if (v.PausedUntil) {
    const off = el("button", "primary", "Снять паузу");
    off.onclick = () => act(() => call("Pause", 0));
    pause.push(off);
  }
  fill($("pause"), pause);

  fill($("projects"), (v.Projects || []).map((p) => {
    const li = el("li", "mono" + (p.Ignored ? " muted" : ""), p.Repo);
    if (p.Ignored) li.append(el("span", "err small", " — брокер не разрешает"));
    return li;
  }));
  if (!v.Projects || !v.Projects.length) fill($("projects"), [el("li", "muted", "Проектов нет: впиши их в файл настроек")]);
  $("autostart").checked = v.Autostart;
  text("version", "cross-review " + v.Version);
}

async function refresh() {
  try {
    render(await call("View"));
  } catch (err) {
    text("status", errorText(err));
  }
}

async function loadRequests() {
  const list = $("requests");
  if (!view || !view.Configured) {
    fill(list, [el("li", "muted small", "Появятся, когда клиент подключится к брокеру")]);
    return;
  }
  if (!list.children.length) fill(list, [el("li", "muted small", "Загружаю…")]);
  try {
    const reqs = await call("Requests");
    if (!reqs || !reqs.length) {
      fill(list, [el("li", "muted small", "Запросов не было")]);
      return;
    }
    fill(list, reqs.map((r) => {
      const li = el("li");
      const head = el("div", "row");
      head.append(el("span", "mono", r.Short), el("span", "muted small", r.When + " · " + r.Status));
      li.append(head, el("div", "muted small", r.Jobs));
      return li;
    }));
  } catch (err) {
    fill(list, [el("li", "err small", errorText(err))]);
  }
}

// act — действие кнопки: ошибку показать в строке состояния, окно обновить.
async function act(fn) {
  try {
    await fn();
  } catch (err) {
    text("status", errorText(err));
  }
  refresh();
}

$("request").addEventListener("submit", async (e) => {
  e.preventDefault();
  const out = $("request-result");
  out.className = "small muted";
  out.textContent = "Отправляю…";
  try {
    out.textContent = await call("Request", $("link").value);
    $("link").value = "";
    loadRequests();
  } catch (err) {
    out.className = "small err";
    out.textContent = errorText(err);
  }
  refresh();
});
$("stop").onclick = () => act(() => call("StopCurrent"));
$("log").onclick = () => call("OpenLastLog");
$("last-log").onclick = () => call("OpenLastLog");
$("settings").onclick = () => act(() => call("OpenSettings"));
$("settings-err-open").onclick = () => act(() => call("OpenSettings"));
$("update-open").onclick = () => call("OpenReleases");
$("requests-reload").onclick = loadRequests;
$("autostart").onchange = (e) => act(() => call("SetAutostart", e.target.checked));
$("quit").onclick = () => call("Quit");

// Окно открыли: свежие данные, мои запросы и ссылка из буфера в пустом поле.
async function opened() {
  await refresh();
  loadRequests();
  if (!$("link").value) {
    const link = await call("ClipboardMR").catch(() => "");
    if (link) $("link").value = link;
  }
}

Events.On("state", refresh);
window.addEventListener("focus", opened);
setInterval(() => { if (view && view.Current) render(view); }, 30000); // минуты текущего ревью
opened();
