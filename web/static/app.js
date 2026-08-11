// Relay 面板的全部 JavaScript。
//
// 面板是服务端渲染的，没有这个文件所有功能也都能正常用——这里只做三件锦上添花
// 的事：危险操作二次确认、防止手抖重复提交、提示条自动淡出。

(function () {
  'use strict';

  // 1. 带 data-confirm 的表单，提交前弹一次确认。
  document.addEventListener('submit', function (e) {
    var form = e.target;
    var msg = form.getAttribute('data-confirm');
    if (msg && !window.confirm(msg)) {
      e.preventDefault();
      return;
    }

    // 2. 提交后把按钮禁掉，防止连点造成重复提交。
    //    延后一拍执行，确保表单数据已经开始发送。
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

  // 3. 成功类提示 6 秒后自动淡出。错误和警告留着不动——那些是用户需要读完的。
  var flash = document.querySelector('.flash-ok');
  if (flash) {
    window.setTimeout(function () {
      flash.style.transition = 'opacity .4s, margin .4s, height .4s';
      flash.style.opacity = '0';
      window.setTimeout(function () { flash.remove(); }, 420);
    }, 6000);
  }
})();
