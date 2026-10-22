/* Web-chat codecs. Pure transport adaptation: mappings remain in the gateway.
 * Fixtures describe supported wire shapes, not a claim of live-site validation.
 */
(() => {
  'use strict';
  const root = typeof window === 'undefined' ? globalThis : window;
  if (root.SILENCER_WEBCHAT) return;
  const LIMIT = 4 * 1024 * 1024;
  const providers = [
    { id: 'chatgpt', domains: ['chatgpt.com'], paths: [/^\/backend-(?:api|anon)\/(?:f\/)?conversation(?:\/prepare)?\/?$/] },
    { id: 'gemini', domains: ['gemini.google.com'], paths: [/^\/_\/BardChatUi\/data\/assistant\.lamda\.BardFrontendService\/StreamGenerate\/?$/] },
    { id: 'deepseek', domains: ['deepseek.com'], paths: [/^\/api\/(?:v\d+\/)?chat\/completion\/?$/] },
    { id: 'doubao', domains: ['doubao.com'], paths: [/^\/samantha\/chat\/completion\/?$/] },
    { id: 'kimi', domains: ['kimi.com', 'kimi.moonshot.cn'], paths: [/^\/api\/chat\/[^/]+\/completion\/?$/, /^\/apiv2\/kimi\.chat\.v1\.ChatService\/Chat\/?$/] },
  ];
  const covers = (host, domain) => host === domain || host.endsWith('.' + domain);
  function providerFor(host) {
    return providers.find((p) => p.domains.some((d) => covers(String(host).toLowerCase(), d))) || null;
  }
  function match(url, page) {
    const target = new URL(url, page);
    const origin = new URL(page);
    const provider = providerFor(origin.hostname);
    if (!provider || !provider.domains.some((d) => covers(target.hostname, d))) return null;
    // Only the known read-only conversation RPC. Other batchexecute methods
    // include settings/authentication operations and stay outside this codec.
    if (provider.id === 'gemini' && target.pathname === '/_/BardChatUi/data/batchexecute' &&
        (target.searchParams.get('rpcids') || '').split(',').includes('hNvQHb')) return { ...provider, history: true };
    if (!provider.paths.some((p) => p.test(target.pathname))) return null;
    return provider.id === 'chatgpt' && /\/prepare\/?$/.test(target.pathname) ? { ...provider, auxiliary: true } : provider;
  }
  function unsupported(code) {
    const error = new TypeError('Silencer: ' + code);
    error.silencerBlocking = true;
    return error;
  }

  // Collect editable text only. Identifiers, CSRF values, model names and auth
  // are never sent for masking. Serialized content objects keep their layers.
  function collect(value, fields, depth = 0) {
    if (!value || typeof value !== 'object' || depth > 24) return;
    for (const key of Object.keys(value)) {
      if (/^(?:metadata|headers|credentials|auth|attachments|files|refs)$/i.test(key)) continue;
      const item = value[key];
      if (typeof item === 'string' && /^(?:prompt|text|content|message|thinking_content|reasoning_content|reply)$/.test(key)) {
        let decoded;
        try { decoded = JSON.parse(item); } catch { /* ordinary text */ }
        if (decoded && typeof decoded === 'object') {
          const nested = [];
          collect(decoded, nested, depth + 1);
          if (nested.length) {
            for (const field of nested) fields.push({ get: field.get, set(next) { field.set(next); value[key] = JSON.stringify(decoded); } });
            continue;
          }
        }
        fields.push({ get: () => value[key], set: (next) => { value[key] = next; } });
      } else if (Array.isArray(item) && key === 'parts') {
        item.forEach((part, index) => {
          if (typeof part === 'string') fields.push({ get: () => item[index], set: (next) => { item[index] = next; } });
          else collect(part, fields, depth + 1);
        });
      } else if (item && typeof item === 'object') collect(item, fields, depth + 1);
    }
  }

  async function maskRequest(provider, raw, mask) {
    if (raw.length > LIMIT) throw unsupported('webchat_request_too_large');
    let fields = [], encode;
    if (provider.id === 'gemini') {
      const pieces = raw.split('&');
      const positions = pieces.map((part, index) => part.split('=')[0] === 'f.req' ? index : -1).filter((i) => i >= 0);
      if (positions.length !== 1) throw unsupported('gemini_request_shape_changed');
      const index = positions[0], offset = pieces[index].indexOf('=');
      let outer, inner;
      try {
        outer = JSON.parse(decodeURIComponent(pieces[index].slice(offset + 1).replace(/\+/g, ' ')));
        if (!provider.history) inner = JSON.parse(outer[1]);
      } catch { throw unsupported('gemini_request_shape_changed'); }
      if (provider.history) {
        if (!Array.isArray(outer) || !outer.some((group) => Array.isArray(group) && group.some((call) => Array.isArray(call) && call[0] === 'hNvQHb'))) throw unsupported('gemini_history_request_shape_changed');
        encode = () => raw; // Read-only IDs/CSRF never enter masking or the ledger.
      } else {
        if (!Array.isArray(inner?.[0]) || typeof inner[0][0] !== 'string') throw unsupported('gemini_request_shape_changed');
        fields = [{ get: () => inner[0][0], set: (next) => { inner[0][0] = next; } }];
        encode = () => {
          outer[1] = JSON.stringify(inner);
          pieces[index] = pieces[index].slice(0, offset + 1) + encodeURIComponent(JSON.stringify(outer));
          return pieces.join('&');
        };
      }
    } else {
      let document;
      try { document = JSON.parse(raw); } catch { throw unsupported('webchat_request_encoding_unsupported'); }
      if (!document || typeof document !== 'object') throw unsupported('webchat_request_shape_changed');
      collect(provider.auxiliary ? document.partial_query?.content : document, fields);
      encode = () => JSON.stringify(document);
    }
    if (provider.auxiliary && !fields.some((field) => field.get().length)) return { ok: true, masked_text: raw, sid: '' };
    if (!fields.length && !provider.history) throw unsupported('webchat_text_fields_not_found');
    const texts = fields.map((field) => field.get());
    const result = await mask(JSON.stringify(texts));
    if (!result?.ok) return result;
    let masked;
    try { masked = JSON.parse(result.masked_text); } catch { throw unsupported('invalid_mask_response'); }
    if (!Array.isArray(masked) || masked.length !== texts.length || masked.some((v) => typeof v !== 'string')) throw unsupported('invalid_mask_response');
    const changed = masked.some((value, index) => value !== texts[index]);
    if (changed) masked.forEach((value, index) => fields[index].set(value));
    return { ...result, masked_text: changed ? encode() : raw };
  }

  // Each logical text channel holds its own partial marker. Never concatenate
  // content and reasoning or IDs. Only complete marker spans reach native Push.
  function createRestorer(provider, contentType, restore, report) {
    // Prepare responses contain transport credentials, not model text. Preserve
    // their bytes and never send them to the restore/ledger API.
    if (provider.auxiliary) return { async push(text) { return text; } };
    let buffer = '', queue = [], queued = 0, prefix = provider.id === 'gemini';
    let recordCount = 0, patchCount = 0;
    let sseFrameCount = 0, sseDoneFrames = 0, sseTerminal = false, sseNonDataFrames = 0, sseMultilineFrames = 0, sseShapeChanges = 0;
    let ssePreservedFrames = 0, sseRewrittenFrames = 0;
    const sseSeparators = { lf: 0, crlf: 0, mixed: 0 };
    const sseEvents = Object.create(null), sseTags = Object.create(null), recentSSEFrames = [];
    const pending = new Map(), patchStates = new Map(), bindings = new Map(), arrayLengths = new Map();
    let arrayBindingCount = 0;
    let wireChannel = 0;
    const chatgptPatchState = { path: '', op: 'ADD' };
    const channelKey = (path) => {
      const key = JSON.stringify(path);
      if (key.length > 8192) throw unsupported('webchat_channel_key_limit');
      return key;
    };
    const pointer = (path) => path ? path.replace(/^\//, '').split('/').map((part) => part.replace(/~1/g, '/').replace(/~0/g, '~')) : [];
    const isOwner = (p) => Array.isArray(p) && (p[0] === 'message' || p[0] === 'fragment');
    const coordinates = (path) => ({ context: path.filter((p) => Array.isArray(p) && !isOwner(p)), owners: path.filter(isOwner), fields: path.filter((p) => !Array.isArray(p)) });
    // Snapshots and JSON-pointer deltas address the same logical field. Keep
    // owner bindings only while needed; an unqualified delta cannot choose
    // between two messages whose partial placeholders are both outstanding.
    const channelFor = (path, patchPointer = null) => {
      const { context, owners, fields } = coordinates(path);
      const target = patchPointer == null ? fields : fields.concat(pointer(patchPointer));
      const field = channelKey([context, target]);
      let candidates = bindings.get(field);
      if (!candidates) { candidates = new Map(); bindings.set(field, candidates); }
      let binding;
      if (patchPointer != null) {
        const matching = [...candidates.values()].filter((item) => owners.every((owner) => item.owners.some((known) => known[0] === owner[0] && known[1] === owner[1])));
        if (matching.length > 1) throw unsupported('webchat_ambiguous_channel');
        binding = matching[0];
      }
      if (!binding) {
        const ownerKey = channelKey(owners);
        binding = candidates.get(ownerKey);
        if (!binding) {
          for (const [id, previous] of candidates) if (!pending.has(previous.key)) candidates.delete(id);
          binding = { owners, key: channelKey([context, owners, target]) };
          candidates.set(ownerKey, binding);
        }
      }
      if (bindings.size > 1024 || candidates.size > 1024) throw unsupported('webchat_channel_limit');
      return binding.key;
    };
    const emitText = async (text, key, set, snapshot = false) => {
      const previous = pending.get(key);
      pending.delete(key);
      if (previous && !snapshot) text = previous.text + text;
      let split = text.length;
      const start = text.lastIndexOf('{{');
      if (start >= 0 && text.indexOf('}}', start) < 0) split = start;
      else if (text.endsWith('{')) split--;
      const tail = text.slice(split);
      if (tail.length > 256) throw unsupported('webchat_placeholder_too_long');
      const head = text.slice(0, split);
      const value = head ? await restore(head) : '';
      if (typeof value !== 'string') throw unsupported('webchat_restore_failed');
      set(value);
      if (tail) pending.set(key, { text: tail, set });
      if (pending.size > 1024) throw unsupported('webchat_channel_limit');
    };

    async function geminiCandidates(candidates, path) {
      if (!Array.isArray(candidates)) return;
      for (const [index, candidate] of candidates.entries()) {
        if (!Array.isArray(candidate)) continue;
        const owner = path.concat([['fragment', String(candidate[0] || index)]]);
        for (const [label, parent, key] of [
          ['text', candidate[1], 0],
          ['card', candidate[22], 0],
          ['thoughts', candidate[37]?.[0], 0],
        ]) {
          if (typeof parent?.[key] === 'string') await emitText(parent[key], channelFor(owner.concat(label)), (next) => { parent[key] = next; }, true);
        }
      }
    }
    async function geminiStructuredText(structured, path) {
      // The observed structured-content plain-text sequence. Other node payloads
      // and adjacent IDs stay untouched; this is not a recursive string rewrite.
      const nodes = structured?.[0]?.[0];
      if (!Array.isArray(nodes)) return;
      for (const [index, node] of nodes.entries()) {
        const payload = node?.[1];
        if (typeof payload?.[2] === 'string') await emitText(payload[2], channelFor(path.concat('node', String(index))), (next) => { payload[2] = next; }, true);
      }
    }
    const observedGeminiPaths = new Map();
    const reportGemini = (inner) => {
      if (typeof report !== 'function') return;
      // Diagnose only positions of remaining marker-shaped values. Never expose
      // their text, IDs, unknown object keys or credentials in page diagnostics.
      const paths = [];
      let visited = 0;
      const scan = (value, path = []) => {
        if (++visited > 8192 || path.length > 24 || paths.length >= 12) return;
        if (typeof value === 'string') {
          if (/\{\{[A-Z0-9_]+_[a-f0-9]{16}\}\}/.test(value)) paths.push({ path, length: value.length });
        } else if (Array.isArray(value)) value.forEach((item, index) => scan(item, path.concat(index)));
        else if (value && typeof value === 'object') Object.values(value).forEach((item) => scan(item, path.concat(':field')));
      };
      scan(inner);
      for (const item of paths) if (observedGeminiPaths.size < 12) observedGeminiPaths.set(JSON.stringify(item.path), item);
      try { report({ history: !!provider.history, remainingMarkerPaths: [...observedGeminiPaths.values()] }); } catch { /* diagnostics cannot affect traffic */ }
    };

    const arrayAt = (path, error = 'webchat_ambiguous_array_append') => {
      const pos = coordinates(path), candidates = arrayLengths.get(channelKey([pos.context, pos.fields]));
      const matching = [...(candidates?.values() || [])].filter((item) => pos.owners.every((owner) => item.owners.some((known) => known[0] === owner[0] && known[1] === owner[1])));
      if (matching.length > 1) throw unsupported(error);
      return matching[0];
    };
    // DeepSeek uses Array.at coordinates, including -1 for the last fragment.
    // Resolve against the original array length, before looking up a text lane.
    const resolveTarget = (path, parts) => {
      const target = path.slice();
      for (const part of parts) {
        if (/^-\d+$/.test(part)) {
          const previous = arrayAt(target, 'webchat_ambiguous_array_index');
          const index = previous && previous.length + Number(part);
          if (!previous || index < 0 || index >= previous.length) throw unsupported('webchat_unknown_array_index');
          target.push(String(index));
        } else target.push(part);
      }
      return target;
    };
    const removeTracked = (target) => {
      const pos = coordinates(target), parentPath = target.slice(0, -1);
      const numeric = /^\d+$/.test(pos.fields.at(-1) || '');
      const parent = numeric ? arrayAt(parentPath, 'webchat_ambiguous_array_remove') : null;
      const index = Number(pos.fields.at(-1));
      if (parent && index >= parent.length) throw unsupported('webchat_unknown_array_remove');
      const parentFields = pos.fields.slice(0, -1);
      const prefix = (left, right) => right.every((part, i) => left[i] === part);
      const rewrite = (context, owners, fields) => {
        if (channelKey(context) !== channelKey(pos.context) || !pos.owners.every((owner) => owners.some((known) => known[0] === owner[0] && known[1] === owner[1]))) return fields;
        if (prefix(fields, pos.fields)) return null;
        if (parent && prefix(fields, parentFields) && /^\d+$/.test(fields[parentFields.length] || '') && Number(fields[parentFields.length]) > index) {
          fields = fields.slice();
          fields[parentFields.length] = String(Number(fields[parentFields.length]) - 1);
        }
        return fields;
      };
      const nextPending = [];
      for (const [key, item] of pending) {
        const [context, owners, fields] = JSON.parse(key), next = rewrite(context, owners, fields);
        if (next) nextPending.push([channelKey([context, owners, next]), item]);
      }
      pending.clear();
      for (const [key, item] of nextPending) pending.set(key, item);
      const nextBindings = [];
      for (const candidates of bindings.values()) for (const binding of candidates.values()) {
        const [context, owners, fields] = JSON.parse(binding.key), next = rewrite(context, owners, fields);
        if (next) nextBindings.push([channelKey([context, next]), channelKey(owners), { ...binding, key: channelKey([context, owners, next]) }]);
      }
      bindings.clear();
      for (const [field, owner, binding] of nextBindings) {
        if (!bindings.has(field)) bindings.set(field, new Map());
        bindings.get(field).set(owner, binding);
      }
      if (parent) parent.length--;
      const nextArrays = [];
      for (const candidates of arrayLengths.values()) for (const item of candidates.values()) {
        const coords = coordinates(item.path), next = rewrite(coords.context, coords.owners, coords.fields);
        if (!next) continue;
        let cursor = 0;
        const path = item.path.map((part) => Array.isArray(part) ? part : next[cursor++]);
        nextArrays.push([channelKey([coords.context, next]), channelKey(coords.owners), { ...item, path }]);
      }
      arrayLengths.clear();
      arrayBindingCount = 0;
      for (const [field, owner, item] of nextArrays) {
        if (!arrayLengths.has(field)) arrayLengths.set(field, new Map());
        arrayLengths.get(field).set(owner, item);
        arrayBindingCount++;
      }
    };
    const trace = [];
    let activeFrameOps = null;
    const diagnosticFields = new Set(['c', 'p', 'o', 'v', 'message', 'response', 'content', 'parts', 'choices', 'event', 'type', 'id', 'message_id', 'conversation_id', 'parent_id', 'children', 'error', 'metadata', 'role', 'status', 'author', 'recipient', 'channel', 'end_turn', 'weight', 'content_type', 'format', 'message_type', 'finish_details', 'current_node', 'mapping', 'messages', 'patch', 'patches']);
    const diagnosticKey = (key) => diagnosticFields.has(key) ? key : ':field';
    const diagnosticShape = (value, depth = 0, budget = { nodes: 0 }) => {
      if (++budget.nodes > 512) return 'limit';
      if (value === null) return 'null';
      if (typeof value === 'string') return 'string';
      if (typeof value === 'number') return 'number';
      if (typeof value === 'boolean') return 'boolean';
      if (typeof value !== 'object') return typeof value;
      if (depth >= 12) return Array.isArray(value) ? 'array:depth' : 'object:depth';
      if (Array.isArray(value)) return ['array', value.length, value.slice(0, 64).map((item) => diagnosticShape(item, depth + 1, budget)), value.length > 64 ? 'more' : ''];
      const keys = Object.keys(value);
      return ['object', keys.length, keys.slice(0, 64).map((key) => [diagnosticKey(key), diagnosticShape(value[key], depth + 1, budget)]), keys.length > 64 ? 'more' : ''];
    };
    const rootShape = (value) => ({
      kind: Array.isArray(value) ? 'array' : value && typeof value === 'object' ? 'object' : value === null ? 'null' : typeof value,
      fields: value && !Array.isArray(value) && typeof value === 'object' ? Object.keys(value).slice(0, 24).map((key) => diagnosticKey(key)) : [],
      arrayLength: Array.isArray(value) ? value.length : undefined,
    });
    const safeEvent = (event) => /^(?:message|delta|done|error|ping|heartbeat|keepalive|close)$/.test(event) ? event : 'other';
    const safeTag = (value) => typeof value === 'string' && /^(?:message|patch|delta|title|status|error|done|metadata|conversation|progress|ping|heartbeat|keepalive|start|end|finish)$/.test(value) ? value : 'other';
    const rememberSSEFrame = (event, kind, details = {}) => {
      sseFrameCount++;
      if (kind !== 'json') sseNonDataFrames++;
      if (kind === 'done') sseDoneFrames++;
      const name = safeEvent(event);
      sseEvents[name] = (sseEvents[name] || 0) + 1;
      if (details.tag) sseTags[details.tag] = (sseTags[details.tag] || 0) + 1;
      recentSSEFrames.push({ index: sseFrameCount, event: name, kind, ...details });
      if (recentSSEFrames.length > 12) recentSSEFrames.shift();
    };
    const safePath = (path) => coordinates(path).fields.map((part) => /^(?:message|response|fragments|content|thinking_content|reasoning_content|parts|choices|text|reply|v|author|recipient|channel|status|end_turn|weight|children|parent_id|metadata|content_type|format|message_type|finish_details|conversation_id|current_node|mapping|messages|patch|patches|\d{1,6})$/.test(part) ? part : ':field').join('/').slice(0, 160);
    const safeOp = (op) => /^(?:SET|ADD|APPEND|BATCH|PATCH|REPLACE|REMOVE)$/.test(op) ? op : 'other';
    const rememberPatch = (target, op, value) => {
      patchCount++;
      const item = { frame: recordCount, path: safePath(target), op: safeOp(op), kind: Array.isArray(value) ? 'array' : typeof value, size: typeof value === 'string' || Array.isArray(value) ? value.length : 0 };
      trace.push(item);
      if (activeFrameOps) activeFrameOps.push(item);
      if (trace.length > 24) trace.shift();
    };
    const valueKind = (node, key) => !node || !Object.prototype.hasOwnProperty.call(node, key) ? 'missing' : node[key] === null ? 'null' : Array.isArray(node[key]) ? 'array' : typeof node[key];
    const patchFailure = (reason, node, target, extra = {}) => {
      const error = unsupported('webchat_patch_shape_changed');
      error.silencerDiagnostic = {
        reason, path: safePath(target), pKind: valueKind(node, 'p'), oKind: valueKind(node, 'o'), vKind: valueKind(node, 'v'),
        patches: trace.slice(), pendingCount: pending.size, ...extra,
      };
      return error;
    };
    const qualify = (value, path) => {
      if (typeof value.event === 'string') path = path.concat([['event', value.event]]);
      if (typeof value.event_type === 'string' || Number.isInteger(value.event_type)) path = path.concat([['event_type', value.event_type]]);
      if (Number.isInteger(value.index)) path = path.concat([['choice', value.index]]);
      const identifier = (v) => typeof v === 'string' || Number.isInteger(v);
      if (identifier(value.message_id)) path = path.concat([['message', value.message_id]]);
      else if (identifier(value.id) && (value.content != null || 'v' in value || typeof value.o === 'string')) path = path.concat([[coordinates(path).fields.includes('fragments') ? 'fragment' : 'message', value.id]]);
      return path;
    };
    async function applyPatch(value, path, depth, state, prefix = []) {
      if (depth > 24) throw unsupported('webchat_patch_depth_limit');
      if (typeof value.p === 'string') state.path = value.p;
      if (typeof value.o === 'string') state.op = value.o.toUpperCase();
      const target = resolveTarget(path, prefix.concat(pointer(state.path)));
      rememberPatch(target, state.op, value.v);
      // DeepSeek BATCH siblings inherit p/o. ChatGPT PATCH children are
      // independent operations relative to the parent target; only top-level
      // ChatGPT deltas inherit compressed c/p/o, never v.
      if (state.op === 'BATCH' || state.op === 'PATCH') {
        if (!Array.isArray(value.v)) throw patchFailure('batch_value_not_array', value, target, { depth, op: safeOp(state.op) });
        const inherited = { path: '', op: 'SET' };
        const base = prefix.concat(pointer(state.path));
        for (const [childIndex, child] of value.v.entries()) {
          const independent = provider.id === 'chatgpt' && state.op === 'PATCH';
          const childState = independent ? { path: '', op: '' } : inherited;
          const remove = provider.id === 'chatgpt' && String(child?.o).toUpperCase() === 'REMOVE';
          if (!child || typeof child !== 'object' || (!('v' in child) && !remove)) throw patchFailure('invalid_batch_child', child, target, { depth, childIndex, op: safeOp(typeof child?.o === 'string' ? child.o.toUpperCase() : childState.op) });
          await applyPatch(child, qualify(child, path), depth + 1, childState, base);
        }
      } else if (provider.id === 'chatgpt' && state.op === 'REMOVE') {
        removeTracked(target);
      } else if (!['ADD', 'REPLACE', 'SET', 'APPEND'].includes(state.op)) {
        // ChatGPT also carries opaque numeric operations inside message
        // metadata. They do not contain editable text; leave them untouched
        // for the site decoder instead of blocking an otherwise safe stream.
        if (provider.id === 'chatgpt' && coordinates(target).fields.includes('metadata') && typeof value.v === 'number') return;
        throw patchFailure('unknown_patch_operation', value, target, { depth, op: safeOp(state.op) });
      } else if (typeof value.v === 'string' && (/^(?:content|text|reply|thinking_content|reasoning_content)$/.test(coordinates(target).fields.at(-1)) ||
                 /parts\/\d+$/.test(coordinates(target).fields.join('/')))) {
        await emitText(value.v, channelFor(target, ''), (next) => { value.v = next; }, state.op !== 'APPEND');
      } else if (value.v && typeof value.v === 'object') {
        let destination = target, start = 0;
        if (Array.isArray(value.v) && state.op === 'APPEND') {
          const previous = arrayAt(target);
          if (!previous) throw unsupported('webchat_unknown_array_append');
          start = previous.length;
          destination = previous.path;
        }
        await walk(value.v, destination, depth + 1, start, state.op !== 'APPEND');
      }
    }

    async function walk(value, path = [], depth = 0, offset = 0, snapshot = false) {
      if (!value || typeof value !== 'object' || depth > 24) return;
      path = qualify(value, path);
      const lane = channelKey(path);
      let handledValue = false;
      if (('v' in value || typeof value.p === 'string' || typeof value.o === 'string') && (provider.id === 'chatgpt' || typeof value.p === 'string' || typeof value.o === 'string' || patchStates.has(lane) || value.v?.message || value.v?.response || value.v?.choices)) {
        let state = provider.id === 'chatgpt' ? chatgptPatchState : patchStates.get(lane);
        if (!state) { state = { path: '', op: 'SET' }; patchStates.set(lane, state); }
        if (patchStates.size > 1024) throw unsupported('webchat_channel_limit');
        await applyPatch(value, path, depth, state);
        handledValue = true;
      }
      if (Array.isArray(value)) {
        const pos = coordinates(path);
        const key = channelKey([pos.context, pos.fields]), ownerKey = channelKey(pos.owners);
        let candidates = arrayLengths.get(key);
        if (!candidates) { candidates = new Map(); arrayLengths.set(key, candidates); }
        if (!candidates.has(ownerKey)) arrayBindingCount++;
        if (arrayBindingCount > 1024) throw unsupported('webchat_channel_limit');
        candidates.set(ownerKey, { length: offset + value.length, path, owners: pos.owners });
      }
      for (const key of Object.keys(value)) {
        if (key === 'v' && handledValue) continue;
        if (/^(?:id|message_id|conversation_id|chat_session_id|parent_id|model|role|p|o|v|url|signature)$/.test(key) && !(key === 'v' && typeof value.v === 'object')) continue;
        const child = value[key], childPath = path.concat(Array.isArray(value) && /^\d+$/.test(key) ? String(Number(key) + offset) : key);
        if (Array.isArray(value) && coordinates(path).fields.at(-1) === 'parts' && typeof child === 'string') {
          await emitText(child, channelFor(childPath), (next) => { value[key] = next; }, true);
          continue;
        }
        if (provider.id === 'gemini' && Array.isArray(value) && value[0] === 'wrb.fr' && key === '2' && typeof child === 'string') {
          if (provider.history && value[1] !== 'hNvQHb') continue;
          let inner;
          try { inner = JSON.parse(child); } catch { throw unsupported('gemini_response_shape_changed'); }
          if (provider.history) {
            const turns = inner?.[0];
            if (Array.isArray(turns)) for (const [index, turn] of turns.entries()) {
              const turnPath = path.concat('history', String(index));
              const user = turn?.[2]?.[0];
              if (typeof user?.[0] === 'string') await emitText(user[0], channelFor(turnPath.concat('user')), (next) => { user[0] = next; }, true);
              await geminiCandidates(turn?.[3]?.[0], turnPath);
              await geminiStructuredText(turn?.[3]?.[12], turnPath.concat('structured'));
            }
          } else {
            await geminiCandidates(inner?.[4], path.concat('gemini'));
            await geminiStructuredText(inner?.[26], path.concat('gemini', 'structured'));
          }
          reportGemini(inner);
          value[key] = JSON.stringify(inner);
        } else if (typeof child === 'string' && /^(?:event_data|content)$/.test(key) && /^[\[{]/.test(child.trim())) {
          let inner;
          try { inner = JSON.parse(child); } catch { /* plain text */ }
          if (inner && typeof inner === 'object') { await walk(inner, childPath, depth + 1, 0, snapshot); value[key] = JSON.stringify(inner); }
          else await emitText(child, channelFor(childPath), (next) => { value[key] = next; }, snapshot);
        } else if (typeof child === 'string' && /^(?:text|content|reply|thinking_content|reasoning_content)$/.test(key)) {
          await emitText(child, channelFor(childPath), (next) => { value[key] = next; }, snapshot);
        } else if (Array.isArray(child) && key === 'parts') {
          await walk(child, childPath, depth + 1, 0, snapshot);
        } else if (child && typeof child === 'object') await walk(child, childPath, depth + 1, 0, snapshot);
      }
    }

    async function record(raw, format, event = 'message') {
      recordCount++;
      let value;
      try { value = JSON.parse(raw); } catch { throw unsupported('webchat_response_shape_changed'); }
      const original = JSON.stringify(value);
      const inputShape = JSON.stringify(diagnosticShape(value));
      const shape = rootShape(value);
      const tag = safeTag(value?.type);
      const frameOps = [];
      const path = [['sse_event', event]];
      // c is the compressed decoder channel, independent from message.channel.
      // Inherit it across records; each channel has separate partial text lanes.
      if (provider.id === 'chatgpt') {
        if (Number.isInteger(value?.c)) wireChannel = value.c;
        path.push(['wire_channel', wireChannel]);
      }
      const previousFrameOps = activeFrameOps;
      activeFrameOps = frameOps;
      try { await walk(value, path); }
      finally { activeFrameOps = previousFrameOps; }
      const shapeChanged = inputShape !== JSON.stringify(diagnosticShape(value));
      const modified = original !== JSON.stringify(value);
      if (shapeChanged) sseShapeChanges++;
      queue.push(() => {
        const body = JSON.stringify(value);
        if (format === 'gemini') return (body.length + 1) + '\n' + body;
        return body;
      });
      return { ...shape, tag, operations: frameOps.slice(0, 12).map((item) => ({ path: item.path, op: item.op, kind: item.kind, size: item.size })), shapeChanged, modified };
    }
    const drain = () => {
      if (pending.size) return '';
      const text = queue.map((f) => f()).join('');
      queue = []; queued = 0;
      return text;
    };
    const diagnostics = () => ({
      provider: provider.id,
      records: recordCount,
      patchCount,
      pending: [...pending].slice(0, 12).map(([key, item]) => ({ path: safePath(JSON.parse(key).flat()), length: item.text.length })),
      recentPatches: trace.slice(),
      ...(contentType.includes('event-stream') ? { sse: {
        frames: sseFrameCount, records: recordCount, doneFrames: sseDoneFrames, controlFrames: sseNonDataFrames,
        multilineDataFrames: sseMultilineFrames, shapeChanges: sseShapeChanges,
        preservedFrames: ssePreservedFrames, rewrittenFrames: sseRewrittenFrames,
        separators: { ...sseSeparators }, events: { ...sseEvents }, tags: { ...sseTags },
        parserBufferChars: buffer.length, queuedFrames: queue.length, queuedChars: queued,
        recent: recentSSEFrames.slice(),
      } } : {}),
    });
    const sse = contentType.includes('event-stream');
    const jsonLines = /ndjson|json-seq/.test(contentType);
    const guard = () => {
      if (queued > LIMIT || queue.length > 2048) throw unsupported('webchat_response_buffer_limit');
    };
    return {
      async push(text, final = false) {
        buffer += text;
        if (buffer.length + queued > LIMIT) throw unsupported('webchat_response_buffer_limit');
        if (sse) {
          let match;
          while ((match = /\r?\n\r?\n/.exec(buffer))) {
            const frame = buffer.slice(0, match.index), separator = match[0];
            buffer = buffer.slice(match.index + separator.length);
            const lines = frame.split(/\r?\n/), dataLines = lines.filter((line) => /^data:/.test(line));
            const data = dataLines.map((line) => line.slice(5).replace(/^ /, '')).join('\n');
            const event = lines.filter((line) => /^event:/.test(line)).at(-1)?.slice(6).replace(/^ /, '') || 'message';
            const separatorKind = separator === '\r\n\r\n' ? 'crlf' : separator.includes('\r') ? 'mixed' : 'lf';
            sseSeparators[separatorKind]++;
            if (!data) {
              rememberSSEFrame(event, 'empty', { lines: lines.length, dataLines: 0 });
              queue.push(() => frame + separator);
            } else if (data === '[DONE]') {
              rememberSSEFrame(event, 'done', { lines: lines.length, dataLines: dataLines.length });
              if (provider.id === 'chatgpt') sseTerminal = true;
              queue.push(() => frame + separator);
            } else {
              const first = lines.findIndex((line) => /^data:/.test(line));
              if (dataLines.length > 1) sseMultilineFrames++;
              const shape = await record(data, 'json', event);
              rememberSSEFrame(event, 'json', { lines: lines.length, dataLines: dataLines.length, separator: separatorKind, ...shape });
              const encode = queue.pop();
              if (!shape.modified && dataLines.length === 1) {
                ssePreservedFrames++;
                const outputFrame = frame + separator;
                queue.push(() => outputFrame);
                queued += outputFrame.length;
              } else {
                sseRewrittenFrames++;
                const body = encode();
                let outputFrame;
                if (dataLines.length === 1) {
                  const line = dataLines[0], at = frame.indexOf(line), colon = line.indexOf(':');
                  const whitespace = line.slice(colon + 1).match(/^\s*/)?.[0] || '';
                  outputFrame = frame.slice(0, at) + line.slice(0, colon + 1) + whitespace + body + frame.slice(at + line.length) + separator;
                } else {
                  outputFrame = lines.flatMap((line, index) => index === first ? ['data: ' + body] : /^data:/.test(line) ? [] : [line]).join('\n') + '\n\n';
                }
                queue.push(() => outputFrame);
                queued += outputFrame.length;
              }
              guard();
              continue;
            }
            queued += frame.length + separator.length;
            guard();
          }
          if (final && buffer.trim()) throw unsupported('webchat_truncated_sse');
        } else if (provider.id === 'gemini') {
          if (prefix) {
            if (buffer.length < 5 && !final) return '';
            if (buffer.startsWith(")]}'")) {
              const end = buffer.indexOf('\n');
              if (end < 0) { if (final) throw unsupported('gemini_truncated_prefix'); return ''; }
              const prelude = buffer.slice(0, end + 1); queue.push(() => prelude); buffer = buffer.slice(end + 1);
            }
            prefix = false;
          }
          while (buffer) {
            const space = /^\s+/.exec(buffer);
            if (space) { const value = space[0]; queue.push(() => value); buffer = buffer.slice(value.length); queued += value.length; guard(); continue; }
            const header = /^(\d+)\r?\n/.exec(buffer);
            if (!header) {
              if (final) { await record(buffer, 'json'); buffer = ''; }
              break;
            }
            const length = Number(header[1]);
            if (length > LIMIT) throw unsupported('gemini_frame_too_large');
            // Google counts UTF-16 units (JS string.length), including the LF
            // after the numeric marker. Unicode bytes are decoded before here.
            const source = buffer.slice(header[0].length - 1);
            if (source.length < length) break;
            await record(source.slice(0, length), 'gemini'); queued += length;
            guard();
            buffer = source.slice(length);
          }
          if (final && buffer) throw unsupported('gemini_truncated_frame');
        } else if (jsonLines) {
          let index;
          while ((index = buffer.indexOf('\n')) >= 0) {
            const line = buffer.slice(0, index).replace(/^\x1e/, ''); buffer = buffer.slice(index + 1);
            if (line.trim()) { await record(line, 'json'); queue.push(() => '\n'); queued += line.length; guard(); }
          }
          if (final && buffer.trim()) { await record(buffer, 'json'); buffer = ''; }
        } else if (buffer.trim()) {
          let complete = false;
          try { JSON.parse(buffer); complete = true; } catch { if (final) throw unsupported('webchat_response_shape_changed'); }
          if (complete) { await record(buffer, 'json'); buffer = ''; }
        }
        if (queued > LIMIT) throw unsupported('webchat_response_buffer_limit');
        if (final && pending.size) {
          const error = unsupported('webchat_incomplete_placeholder');
          // Structure only: no message IDs, text, placeholders or credentials.
          error.silencerDiagnostic = { patches: trace, pending: [...pending].slice(0, 12).map(([key, item]) => ({ path: safePath(JSON.parse(key).flat()), length: item.text.length })) };
          throw error;
        }
        return drain();
      },
      isTerminal: () => sseTerminal,
      diagnostics,
    };
  }
  root.SILENCER_WEBCHAT = Object.freeze({ providers, providerFor, match, maskRequest, createRestorer });
})();
