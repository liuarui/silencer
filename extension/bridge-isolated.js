/* Silencer Browser Bridge — ISOLATED world 中继层
 *
 * 唯一职责：把 MAIN world 的 postMessage 请求转成 chrome.runtime.sendMessage 发给 SW，
 * 再把 SW 的响应原路回程。**不做任何判定、不做任何缓存、不碰 token**。
 *
 * 安全边界：
 *  - 只接受同源（e.origin === location.origin）且 type === 'SILENCER_BRIDGE_REQ' 的消息；
 *  - 回程 targetOrigin 用 location.origin（不用 '*'），避免把结果广播给别的源；
 *  - 页面可伪造同源 postMessage —— 无害：打码只进不出；sid 由服务端签发、SW 再按 tab 校验，
 *    payload 里的 host/sid 一律由 SW 覆写/忽略。ISOLATED↔MAIN 一次性握手密钥列 v1.1。
 */

(() => {
  'use strict';
  // content_scripts 的 all_frames:true 会让每个 frame 各跑一份，用 guard 防重复注入
  if (window.__SILENCER_ISOLATED__) return;
  window.__SILENCER_ISOLATED__ = true;

  window.addEventListener('message', (e) => {
    if (e.origin !== location.origin) return;
    if (!e.data || e.data.type !== 'SILENCER_BRIDGE_REQ') return;

    const nonce = e.data.nonce;
    const action = e.data.action;
    const payload = e.data.payload;
    if (typeof action !== 'string') return;

    let replied = false;
    const reply = (result) => {
      // notify 分支（nonce == null）不需要回程；已回程过也不重复回
      if (replied || nonce == null) return;
      replied = true;
      window.postMessage({ type: 'SILENCER_BRIDGE_RESP', nonce, result }, location.origin);
    };

    try {
      chrome.runtime.sendMessage({ action, payload }, (resp) => {
        // SW 未响应 / 扩展刚被重载更新 → lastError 存在、resp 为 undefined。
        // 回 null 交给 MAIN 侧按 (B) 默认桶直通处理，**绝不让页面请求悬挂**。
        if (chrome.runtime.lastError) {
          reply(null);
          return;
        }
        reply(resp === undefined ? null : resp);
      });
    } catch (err) {
      // 扩展上下文已失效（reload / update / 卸载），此时 sendMessage 直接抛。
      // 同上：回 null，页面照常走原生 fetch。
      reply(null);
    }
  });
})();
