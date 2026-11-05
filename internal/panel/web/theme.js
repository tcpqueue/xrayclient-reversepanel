/* 跟随系统亮/暗色（在 <head> 中同步加载，避免闪烁；单独成文件以便 CSP 禁止内联脚本） */
(function () {
  var m = window.matchMedia("(prefers-color-scheme: dark)");
  var set = function () { document.documentElement.setAttribute("data-bs-theme", m.matches ? "dark" : "light"); };
  set(); m.addEventListener("change", set);
})();
