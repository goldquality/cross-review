// Отрисовка без HTML-разбора: всё, что пришло из Go (а там бывают строки брокера), попадает в DOM
// только через textContent. Свойств и методов, которые разбирают строку как разметку, в этих файлах
// нет — CI проверяет это grep'ом (список — в .github/workflows/proto-wails.yml).

export function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = String(text);
  return e;
}

export function fill(parent, children) {
  parent.replaceChildren(...children);
}

export function text(id, value) {
  document.getElementById(id).textContent = value ?? "";
}
