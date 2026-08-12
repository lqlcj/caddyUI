// CaddyUI 面板的全部 JavaScript。
//
// 面板是服务端渲染的，没有这个文件所有功能也都能用 —— 主题切换会退化成一次
// 普通的表单提交，复制按钮会消失（路径本身还看得见、能手动选中）。
// 这里只做几件锦上添花的事：主题即时切换、复制路径、Docker 配置预览与按需加载、
// 危险操作二次确认和防止手抖重复提交。

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
    var portLink = e.target.closest ? e.target.closest('[data-open-port]') : null;
    if (portLink) {
      e.preventDefault();
      var port = portLink.getAttribute('data-open-port');
      if (/^\d{1,5}$/.test(port || '')) {
        var host = window.location.hostname;
        if (host.indexOf(':') !== -1 && host.charAt(0) !== '[') host = '[' + host + ']';
        window.open('http://' + host + ':' + port, '_blank', 'noopener');
      }
      return;
    }

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

  // ---------- 3. Docker 简易设置同步 ----------

  Array.prototype.forEach.call(document.querySelectorAll('[data-docker-editor]'), function (form) {
    var composeEditor = form.querySelector('[data-compose-editor]');
    var envEditor = form.querySelector('[data-env-editor]');
    var status = form.querySelector('[data-compose-sync-status]');
    var syncButton = form.querySelector('[data-compose-sync]');
    if (!composeEditor) return;

    var AUTO_SYNC_LIMIT = 200 * 1024;
    var timer = 0;
    var controller = null;
    var previewURL = '/docker/preview';
    var pending = false;
    var dirty = false;
    var hasSynchronized = false;
    var manualSync = composeEditor.value.length > AUTO_SYNC_LIMIT;
    var canSync = !!(window.fetch && window.FormData && window.URLSearchParams);

    Array.prototype.forEach.call(form.querySelectorAll('[data-compose-port], [data-compose-env], [data-env-setting]'), function (input) {
      input.dataset.syncValue = input.value;
    });

    function relevant(input) {
      return input && (input.hasAttribute('data-compose-port') ||
        input.hasAttribute('data-compose-env') || input.hasAttribute('data-env-setting'));
    }

    function originalFor(input) {
      if (input.hasAttribute('data-compose-port')) {
        return form.querySelector('[data-compose-port-original="' + input.getAttribute('data-compose-port') + '"]');
      }
      var originalName = input.name.replace(/^cenv__/, 'cenvorig__').replace(/^env__/, 'envorig__');
      return form.elements[originalName];
    }

    function showStatus(text, error) {
      if (!status) return;
      status.textContent = text;
      status.classList.toggle('err-text', !!error);
    }

    function updateSyncMode() {
      manualSync = composeEditor.value.length > AUTO_SYNC_LIMIT;
      if (syncButton) syncButton.hidden = !manualSync || !dirty;
      if (!dirty) return;
      if (manualSync) {
        showStatus('Compose 较大，已关闭实时同步；可点按钮预览，提交时也会自动应用。', false);
      }
    }

    function synchronize() {
      if (!canSync) {
        pending = false;
        showStatus('浏览器不支持实时同步，提交时仍会按上面的设置生成配置。', false);
        return;
      }
      pending = true;
      if (controller) controller.abort();
      controller = typeof AbortController !== 'undefined' ? new AbortController() : null;
      showStatus('正在同步完整配置…', false);
      var options = {
        method: 'POST',
        body: new URLSearchParams(new FormData(form)),
        credentials: 'same-origin',
        headers: {
          'Accept': 'application/json',
          'Content-Type': 'application/x-www-form-urlencoded;charset=UTF-8'
        }
      };
      if (controller) options.signal = controller.signal;
      window.fetch(previewURL, options).then(function (response) {
        return response.json().then(function (data) {
          if (!response.ok) throw new Error(data.error || '同步失败');
          return data;
        });
      }).then(function (data) {
        if (composeEditor.value !== data.compose) {
          composeEditor.value = data.compose || '';
          composeEditor.dispatchEvent(new Event('input', { bubbles: true }));
        }
        if (envEditor && envEditor.value !== data.env) {
          envEditor.value = data.env || '';
          envEditor.dispatchEvent(new Event('input', { bubbles: true }));
        }
        Array.prototype.forEach.call(form.querySelectorAll('[data-compose-port], [data-compose-env], [data-env-setting]'), function (input) {
          input.dataset.syncValue = input.value;
        });
        hasSynchronized = true;
        dirty = false;
        updateSyncMode();
        pending = false;
        showStatus('已同步到完整 Compose 配置。', false);
      }).catch(function (err) {
        if (err && err.name === 'AbortError') return;
        pending = false;
        showStatus(err && err.message ? err.message : '同步失败，请检查填写内容。', true);
      });
    }

    form.addEventListener('input', function (e) {
      if (!relevant(e.target)) return;
      dirty = true;
      updateSyncMode();
      window.clearTimeout(timer);
      if (!manualSync) timer = window.setTimeout(synchronize, 350);
    });
    form.addEventListener('change', function (e) {
      if (!relevant(e.target)) return;
      dirty = true;
      updateSyncMode();
      window.clearTimeout(timer);
      if (!manualSync) synchronize();
    });
    if (syncButton) syncButton.addEventListener('click', synchronize);
    form.addEventListener('submit', function (e) {
      if (pending) {
        e.preventDefault();
        showStatus('请等完整配置同步完成后再提交。', true);
        return;
      }
      if (hasSynchronized) {
        Array.prototype.forEach.call(form.querySelectorAll('[data-compose-port], [data-compose-env], [data-env-setting]'), function (input) {
          if (input.dataset.syncValue === input.value) {
            var original = originalFor(input);
            if (original) original.value = input.value;
          }
        });
      }
    });

    var needsInitialSync = false;
    Array.prototype.forEach.call(form.querySelectorAll('[data-compose-env], [data-env-setting]'), function (input) {
      var original = originalFor(input);
      if (original && original.value !== input.value) needsInitialSync = true;
    });
    if (needsInitialSync) {
      dirty = true;
      updateSyncMode();
      if (!manualSync) synchronize();
    } else {
      if (manualSync) {
        showStatus('Compose 较大，修改常用设置后可手动同步；提交时也会自动应用。', false);
      } else {
        showStatus('修改上面的设置后，会自动同步到完整 Compose 和 .env。', false);
      }
    }
  });

  // ---------- 4. Docker 日志与 Compose 按需加载 ----------

  Array.prototype.forEach.call(document.querySelectorAll('[data-lazy-text]'), function (box) {
    var url = box.getAttribute('data-lazy-text-url');
    var name = box.getAttribute('data-lazy-text-name') || '内容';
    var button = box.querySelector('[data-lazy-text-load]');
    var status = box.querySelector('[data-lazy-text-status]');
    var output = box.querySelector('[data-lazy-text-output]');
    var loading = false;
    var loaded = false;
    var controller = null;
    if (!url || !button || !output) return;

    function loadText(e) {
      if (!window.fetch) return;
      if (e) e.preventDefault();
      if (loading || loaded) return;
      loading = true;
      button.textContent = '加载中…';
      if (status) status.textContent = '正在读取' + name + '…';
      controller = typeof AbortController !== 'undefined' ? new AbortController() : null;
      var options = { credentials: 'same-origin', headers: { 'Accept': 'text/plain' } };
      if (controller) options.signal = controller.signal;
      window.fetch(url, options).then(function (response) {
        return response.text().then(function (text) {
          if (!response.ok) throw new Error(text || ('读取' + name + '失败'));
          return text;
        });
      }).then(function (text) {
        output.textContent = text;
        output.hidden = false;
        if (status) status.hidden = true;
        button.textContent = '已加载';
        button.hidden = true;
        loaded = true;
        loading = false;
      }).catch(function (err) {
        if (err && err.name === 'AbortError') return;
        loading = false;
        button.textContent = '重新加载';
        if (status) {
          status.hidden = false;
          status.textContent = err && err.message ? err.message : ('读取' + name + '失败');
          status.classList.add('err-text');
        }
      });
    }

    button.addEventListener('click', loadText);
    if (box.tagName === 'DETAILS') {
      box.addEventListener('toggle', function () {
        if (box.open && !loaded) loadText();
      });
    }
    box._cancelLazyText = function () {
      if (controller) controller.abort();
    };
  });

  // Docker 编辑和详情页有大文本。离开时立即断开引用并清空 DOM；服务端也给这些
  // 页面发 no-store，避免浏览器把整页留在前进/后退缓存里。
  if (document.querySelector('[data-docker-heavy-page]')) {
    var releaseDockerText = function () {
      Array.prototype.forEach.call(document.querySelectorAll('[data-lazy-text]'), function (box) {
        if (box._cancelLazyText) box._cancelLazyText();
      });
      Array.prototype.forEach.call(document.querySelectorAll('[data-release-text]'), function (node) {
        if (typeof node.value === 'string') node.value = '';
        else node.textContent = '';
      });
    };
    window.addEventListener('pagehide', releaseDockerText, { once: true });
    window.addEventListener('pageshow', function (e) {
      if (e.persisted) window.location.reload();
    });
  }

  // ---------- 5 & 6. 表单确认与防重复提交 ----------

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

  // ---------- 7. 成功提示自动淡出 ----------
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
