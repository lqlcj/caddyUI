// CaddyUI 面板的全部 JavaScript。
//
// 面板是服务端渲染的，没有这个文件所有功能也都能用 —— 主题切换会退化成一次
// 普通的表单提交，复制按钮会消失（路径本身还看得见、能手动选中）。
// 这里只做四件锦上添花的事：主题即时切换、复制路径、危险操作二次确认、
// 防止手抖重复提交。

(function () {
  'use strict';

  var THEME_COOKIE = 'caddyui_theme';

  // ---------- 1. 主题切换 ----------
  //
  // 服务端已经根据 cookie 把 data-theme 写进 <html> 了，所以首屏不会闪。
  // 这里拦下表单提交，就地改属性 + 写 cookie，页面不用整个重载。

  function currentTheme() {
    var m = document.cookie.match(/(?:^|;\s*)caddyui_theme=(light|dark)/);
    if (m) return m[1];
    // 没有 cookie 时与服务端保持一致，默认使用深色主题。
    return 'dark';
  }

  function applyTheme(theme) {
    document.documentElement.setAttribute('data-theme', theme);
    // 一年后过期。SameSite=Lax 和服务端设的那份保持一致。
    document.cookie = THEME_COOKIE + '=' + theme +
                      ';path=/;max-age=31536000;samesite=lax';
  }

  function syncToggle(form, theme) {
    var next = theme === 'dark' ? 'light' : 'dark';
    var input = form.querySelector('input[name="theme"]');
    if (input) input.value = next;
    var icon = form.querySelector('[data-theme-icon]');
    // 显示的是「点下去会变成什么」：当前深色就显示太阳。
    if (icon) icon.textContent = theme === 'dark' ? '☀' : '☾';
  }

  var themeForms = document.querySelectorAll('[data-theme-form]');
  Array.prototype.forEach.call(themeForms, function (form) {
    syncToggle(form, currentTheme());

    form.addEventListener('submit', function (e) {
      e.preventDefault();
      var input = form.querySelector('input[name="theme"]');
      var next = input ? input.value : 'dark';
      applyTheme(next);
      syncToggle(form, next);
    });
  });

  // ---------- 2. 复制按钮 ----------

  document.addEventListener('click', function (e) {
    var btn = e.target.closest ? e.target.closest('[data-copy], [data-copy-target]') : null;
    if (!btn) return;

    var targetSelector = btn.getAttribute('data-copy-target');
    var target = targetSelector ? document.querySelector(targetSelector) : null;
    var text = target
      ? (typeof target.value === 'string' ? target.value : target.textContent)
      : btn.getAttribute('data-copy');
    if (text === null) return;
    var done = function () {
      var original = btn.dataset.originalText || btn.textContent;
      btn.dataset.originalText = original;
      btn.textContent = '已复制';
      btn.classList.add('done');
      window.setTimeout(function () {
        btn.textContent = original;
        btn.classList.remove('done');
      }, 1600);
    };

    // navigator.clipboard 只在 https 或 localhost 下可用。面板经常是
    // http://IP:81 直接访问的，那种情况下走下面这条老路子。
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(done, fallback);
    } else {
      fallback();
    }

    function fallback() {
      var ta = document.createElement('textarea');
      ta.value = text;
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand('copy'); done(); } catch (err) { /* 复制不了就算了 */ }
      document.body.removeChild(ta);
    }
  });

  // ---------- 3 & 4. 表单确认与防重复提交 ----------

  document.addEventListener('submit', function (e) {
    var form = e.target;
    if (form.hasAttribute('data-theme-form')) return; // 主题表单上面已经处理过

    var msg = form.getAttribute('data-confirm');
    if (msg && !window.confirm(msg)) {
      e.preventDefault();
      return;
    }

    // 提交后把按钮禁掉，防止连点造成重复提交。
    // 延后一拍执行，确保表单数据已经开始发送。
    var btn = form.querySelector('button[type="submit"], button:not([type])');
    if (btn && !e.defaultPrevented) {
      window.setTimeout(function () {
        btn.disabled = true;
        if (!btn.dataset.busyText) {
          btn.dataset.busyText = '1';
          btn.textContent = '处理中…';
        }
      }, 0);
    }
  });

  // ---------- 5. 成功提示自动淡出 ----------
  // 错误和警告留着不动 —— 那些是用户需要读完的。

  var flash = document.querySelector('.flash-ok');
  if (flash) {
    window.setTimeout(function () {
      flash.style.transition = 'opacity .4s, margin .4s, height .4s';
      flash.style.opacity = '0';
      window.setTimeout(function () { flash.remove(); }, 420);
    }, 6000);
  }
})();
