// O06 sanitizer: a pure transform over rrweb 2.x events that applies the
// SAME privacy stack the structural recorder (cmd/observe/tracker/
// observe-replay.js) has shipped since F37/TO-028/TO-030:
//
//   - attribute ALLOWLIST on every node's attributes object (full-snapshot
//     nodes, incremental attribute mutations, and nodes riding addedNodes),
//     with img src sanitized to origin+path and every other URL-bearing
//     attribute (href, srcset, style, data-*, on*) dropped;
//   - private-tag elements (input/textarea/select/option/script/noscript/
//     iframe/object/embed/style/head/meta/link/base/title), contenteditable
//     regions, and data-observe-block subtrees replaced by opaque
//     placeholder nodes (element id preserved so later incremental events
//     stay well-formed);
//   - text bounded to 2048 chars, and characterData mutation values masked
//     (set to the empty string) when the target id lives inside a private
//     subtree;
//   - the Meta event's href (rrweb records location.href verbatim, query
//     string included) reduced to origin+path.
//
// The transform is a pure fold: sanitizeEventWith(state, event) returns the
// next state plus the sanitized event (or null to drop it). The only state
// is the set of node ids that live inside private/blocked subtrees — ids
// rrweb mints monotonically per recording segment (a checkout/full snapshot
// starts the id counter over, and the fold resets with it) — plus the last
// sanitized Meta href, used as the base URL for relative image srcs. No
// DOM access, no globals, no side effects: the same module runs in the
// recorder (before anything leaves the browser) and again in the player
// (the second, independent F38-style layer).
//
// rrweb constants (verified against rrweb 2.1.6 dist):
// NodeType Document=0 DocumentType=1 Element=2 Text=3 CDATA=4 Comment=5;
// EventType DomContentLoaded=0 Load=1 FullSnapshot=2 IncrementalSnapshot=3
// Meta=4 Custom=5 Plugin=6 Asset=7; IncrementalSource Mutation=0 MouseMove=1
// MouseInteraction=2 Scroll=3 ViewportResize=4 Input=5 TouchMove=6
// MediaInteraction=7 StyleSheetRule=8 CanvasMutation=9 Font=10 Log=11
// Drag=12 StyleDeclaration=13 Selection=14 AdoptedStyleSheet=15
// CustomElement=16.

const NODE_DOCUMENT = 0;
const NODE_DOCUMENT_TYPE = 1;
const NODE_ELEMENT = 2;
const NODE_TEXT = 3;
const NODE_CDATA = 4;
const NODE_COMMENT = 5;

const EV_DOM_CONTENT_LOADED = 0;
const EV_LOAD = 1;
const EV_FULL_SNAPSHOT = 2;
const EV_INCREMENTAL = 3;
const EV_META = 4;

const SRC_MUTATION = 0;
const SRC_MOUSE_MOVE = 1;
const SRC_MOUSE_INTERACTION = 2;
const SRC_SCROLL = 3;
const SRC_VIEWPORT_RESIZE = 4;
const SRC_INPUT = 5;
const SRC_TOUCH_MOVE = 6;
const SRC_MEDIA_INTERACTION = 7;
const SRC_DRAG = 12;
const SRC_SELECTION = 14;

// The private-tag set is observe-replay.js's PRIVATE_TAG, verbatim.
const PRIVATE_TAG = /^(input|textarea|select|option|script|noscript|iframe|object|embed|style|head|meta|link|base|title)$/;

// observe-replay.js's CAPTURE_ATTRS, verbatim: only structural/styling-safe
// attributes survive; href/src are false (dropped) except the img-src
// special case in captureAttribute.
const CAPTURE_ATTRS = {
  'class': true, 'id': true, 'colspan': true, 'rowspan': true,
  'dir': true, 'lang': true, 'alt': true, 'width': true, 'height': true,
  'type': true, 'role': true, 'href': false, 'src': false
};

const MAX_TEXT_LENGTH = 2048;
const MAX_SNAPSHOT_NODES = 5000;
const MAX_SNAPSHOT_DEPTH = 32;
const MAX_CHILDREN = 200;

function isPlainObject(v) {
  return v !== null && typeof v === 'object' && !Array.isArray(v);
}

// captureAttribute ports observe-replay.js's rule: img src is sanitized to
// origin+path (no credentials, query, or fragment; <=2048 chars; http/https
// only), everything else must be on the allowlist and a short string.
// baseURL resolves relative srcs and comes from the fold state (the last
// sanitized Meta href) — never from a live location.
function captureAttribute(tag, name, value, baseURL) {
  name = String(name || '').toLowerCase();
  if (name === 'src' && tag === 'img') {
    if (typeof value !== 'string') return null;
    try {
      var u = new URL(value, baseURL || undefined);
      if (u.protocol !== 'https:' && u.protocol !== 'http:') return null;
      u.username = '';
      u.password = '';
      u.search = '';
      u.hash = '';
      return u.href.length <= 2048 ? u.href : null;
    } catch (e) {
      return null;
    }
  }
  if (!CAPTURE_ATTRS[name] || CAPTURE_ATTRS[name] === false) return null;
  if (typeof value !== 'string' || value.length > 256) return null;
  return value;
}

// sanitizePageURL reduces a recorded page URL to origin+path — the same
// boundary observe-replay.js's pageContext() and the server's R28
// CapturedURL enforce. Anything unparseable or non-http(s) becomes ''.
function sanitizePageURL(raw) {
  if (typeof raw !== 'string' || raw === '') return '';
  try {
    var u = new URL(raw);
    if (u.protocol !== 'https:' && u.protocol !== 'http:') return '';
    u.username = '';
    u.password = '';
    u.search = '';
    u.hash = '';
    return u.href;
  } catch (e) {
    return '';
  }
}

// isPrivateAttrs decides privacy from the SERIALIZED attributes rrweb
// emitted (data-observe-block and contenteditable ride the attributes
// object; the sanitizer reads them for the decision and then drops them
// from the output like every other non-allowlisted attribute).
function isPrivateAttrs(tag, attrs) {
  if (PRIVATE_TAG.test(tag)) return true;
  if (attrs) {
    if ('data-observe-block' in attrs) return true;
    var ce = attrs['contenteditable'];
    if (ce !== undefined && ce !== 'false') return true;
  }
  return false;
}

function placeholderElement(node) {
  var out = { type: NODE_ELEMENT, tagName: 'div', attributes: {}, childNodes: [] };
  if (typeof node.id === 'number') out.id = node.id;
  if (node.isSVG === true) out.isSVG = true;
  return out;
}

// collectPrivateIds walks a subtree that will NOT be emitted and records
// every element/text id in it, so later characterData or attribute
// mutations against those ids are masked rather than trusted.
function collectPrivateIds(node, privateIds, budget) {
  if (!isPlainObject(node)) return;
  if (typeof node.id === 'number') privateIds.add(node.id);
  if (budget.limited()) return;
  var children = node.childNodes;
  if (Array.isArray(children)) {
    for (var i = 0; i < Math.min(children.length, MAX_CHILDREN); i++) {
      collectPrivateIds(children[i], privateIds, budget);
    }
  }
}

function boundText(value) {
  if (typeof value !== 'string') return '';
  return value.length > MAX_TEXT_LENGTH ? value.slice(0, MAX_TEXT_LENGTH) : value;
}

// sanitizeNode sanitizes one serialized node (and, recursively, its
// subtree). Private subtrees and over-budget/over-depth subtrees become a
// single opaque placeholder div with the root's id preserved — rrweb's
// replayer tolerates ids whose children are missing, and keeping the id
// keeps subsequent adds/removes targeting this node well-formed.
function sanitizeNode(node, depth, ctx) {
  if (!isPlainObject(node)) return null;
  if (node.type === NODE_TEXT) {
    ctx.nodes++;
    var outText = { type: NODE_TEXT, textContent: boundText(node.textContent) };
    // The id is load-bearing: characterData mutations target text nodes by
    // id, and the replayer's mirror must find the rebuilt node.
    if (typeof node.id === 'number') outText.id = node.id;
    if (typeof node.rootId === 'number') outText.rootId = node.rootId;
    return outText;
  }
  if (node.type !== NODE_ELEMENT) {
    // Document/DocumentType handled by callers; CDATA and comments are
    // dropped (the structural serializer never captured them either).
    return null;
  }

  ctx.nodes++;
  var tag = String(node.tagName || '').toLowerCase();
  var attrs = isPlainObject(node.attributes) ? node.attributes : {};

  if (isPrivateAttrs(tag, attrs) || depth > MAX_SNAPSHOT_DEPTH || ctx.nodes > MAX_SNAPSHOT_NODES) {
    var ids = ctx.privateIds;
    collectPrivateIds(node, ids, ctx.budget);
    // The placeholder's own id is private too — later text mutations on it
    // must mask (a blocked div's own textContent can be set by the page).
    if (typeof node.id === 'number') ids.add(node.id);
    return placeholderElement(node);
  }

  var outAttrs = {};
  for (var name in attrs) {
    if (!Object.prototype.hasOwnProperty.call(attrs, name)) continue;
    // Event handlers and data-* attributes never serialize (token-bearing),
    // matching the structural recorder's serializer loop.
    if (name.lastIndexOf('on', 0) === 0) continue;
    if (name.lastIndexOf('data-', 0) === 0) continue;
    if (name === 'contenteditable') continue;
    var captured = captureAttribute(tag, name, attrs[name], ctx.baseURL);
    if (captured !== null) outAttrs[name] = captured;
  }

  var children = Array.isArray(node.childNodes) ? node.childNodes : [];
  var out = {
    type: NODE_ELEMENT,
    tagName: node.tagName,
    attributes: outAttrs,
    childNodes: []
  };
  if (typeof node.id === 'number') out.id = node.id;
  if (node.isSVG === true) out.isSVG = true;
  if (node.needBlock === true) out.needBlock = true;

  for (var i = 0; i < Math.min(children.length, MAX_CHILDREN); i++) {
    if (ctx.nodes > MAX_SNAPSHOT_NODES) break;
    var child = sanitizeNode(children[i], depth + 1, ctx);
    if (child !== null) out.childNodes.push(child);
  }
  return out;
}

function sanitizeDocumentNode(node, ctx) {
  if (!isPlainObject(node) || node.type !== NODE_DOCUMENT) return null;
  ctx.nodes++;
  var out = { type: NODE_DOCUMENT, childNodes: [] };
  if (typeof node.id === 'number') out.id = node.id;
  var children = Array.isArray(node.childNodes) ? node.childNodes : [];
  for (var i = 0; i < Math.min(children.length, MAX_CHILDREN); i++) {
    if (ctx.nodes > MAX_SNAPSHOT_NODES) break;
    var c = children[i];
    if (isPlainObject(c) && c.type === NODE_DOCUMENT_TYPE) {
      ctx.nodes++;
      var dt = { type: NODE_DOCUMENT_TYPE, name: String(c.name || 'html') };
      if (typeof c.publicId === 'string') dt.publicId = c.publicId;
      if (typeof c.systemId === 'string') dt.systemId = c.systemId;
      if (typeof c.id === 'number') dt.id = c.id;
      out.childNodes.push(dt);
      continue;
    }
    var s = sanitizeNode(c, 1, ctx);
    if (s !== null) out.childNodes.push(s);
  }
  return out;
}

// sanitizeIncrementalData handles one IncrementalSnapshot payload by
// source. Returns the sanitized data, or null to drop the whole event.
function sanitizeIncrementalData(data, state) {
  var source = data.source;

  if (source === SRC_MUTATION) {
    var next = { source: SRC_MUTATION };
    var changed = false;

    if (Array.isArray(data.texts)) {
      next.texts = data.texts.map(function (t) {
        if (!isPlainObject(t) || typeof t.id !== 'number') return null;
        // characterData mutation: masked to the empty string when the id
        // lives in a private subtree (input value, blocked region,
        // contenteditable); otherwise bounded visible text, same policy as
        // snapshots.
        if (state.privateIds.has(t.id)) return { id: t.id, value: '' };
        return { id: t.id, value: boundText(t.value) };
      }).filter(function (t) { return t !== null; });
      changed = true;
    }

    if (Array.isArray(data.attributes)) {
      next.attributes = data.attributes.map(function (a) {
        if (!isPlainObject(a) || typeof a.id !== 'number' || !isPlainObject(a.attributes)) return null;
        var tag = String(a.tagName || '').toLowerCase();
        // rrweb attribute mutations do not carry tagName; the id is all we
        // have, so the allowlist runs tag-less (img-src special casing is
        // unavailable here — a mutated img src is dropped, not proxied).
        var out = {};
        for (var name in a.attributes) {
          if (!Object.prototype.hasOwnProperty.call(a.attributes, name)) continue;
          if (name.lastIndexOf('on', 0) === 0) continue;
          if (name.lastIndexOf('data-', 0) === 0) continue;
          if (name === 'contenteditable') {
            if (a.attributes[name] !== 'false') state.privateIds.add(a.id);
            continue;
          }
          var captured = captureAttribute(tag, name, a.attributes[name], state.baseURL);
          if (captured !== null) out[name] = captured;
        }
        return { id: a.id, attributes: out };
      }).filter(function (a) { return a !== null; });
      changed = true;
    }

    if (Array.isArray(data.removes)) {
      next.removes = data.removes.map(function (r) {
        if (!isPlainObject(r) || typeof r.id !== 'number') return null;
        return { id: r.id, parentId: typeof r.parentId === 'number' ? r.parentId : undefined };
      }).filter(function (r) { return r !== null; });
      changed = true;
    }

    if (Array.isArray(data.adds)) {
      next.adds = data.adds.map(function (a) {
        if (!isPlainObject(a) || !isPlainObject(a.node)) return null;
        var parentId = typeof a.parentId === 'number' ? a.parentId : null;
        var parentPrivate = parentId !== null && state.privateIds.has(parentId);
        var tag = String(a.node.tagName || '').toLowerCase();
        var attrs = isPlainObject(a.node.attributes) ? a.node.attributes : {};
        var nodePrivate = a.node.type === NODE_ELEMENT && isPrivateAttrs(tag, attrs);
        var out = {
          parentId: parentId === null ? undefined : parentId,
          node: null
        };
        if (typeof a.nextId === 'number') out.nextId = a.nextId;
        if (parentPrivate || nodePrivate) {
          collectPrivateIds(a.node, state.privateIds, unlimitedBudget());
          if (typeof a.node.id === 'number') state.privateIds.add(a.node.id);
          out.node = placeholderElement(a.node);
        } else {
          out.node = sanitizeNode(a.node, 1, snapshotCtx(state));
        }
        return out;
      }).filter(function (a) { return a !== null; });
      changed = true;
    }

    if (!changed) return null;
    return next;
  }

  if (source === SRC_MOUSE_MOVE || source === SRC_TOUCH_MOVE || source === SRC_DRAG) {
    if (!Array.isArray(data.positions)) return null;
    return {
      source: source,
      positions: data.positions.map(function (p) {
        if (!isPlainObject(p)) return null;
        var out = { x: num(p.x), y: num(p.y) };
        if (typeof p.id === 'number') out.id = p.id;
        if (typeof p.timeOffset === 'number') out.timeOffset = p.timeOffset;
        return out;
      }).filter(function (p) { return p !== null; })
    };
  }

  if (source === SRC_MOUSE_INTERACTION) {
    // Flat shape {type, id, x, y, pointerType} — coordinates and ids only,
    // no text payload exists on this source.
    var mi = { type: num(data.type) };
    if (typeof data.id === 'number') mi.id = data.id;
    if (typeof data.x === 'number') mi.x = data.x;
    if (typeof data.y === 'number') mi.y = data.y;
    if (typeof data.pointerType === 'number') mi.pointerType = data.pointerType;
    return mi;
  }

  if (source === SRC_SCROLL || source === SRC_VIEWPORT_RESIZE ||
      source === SRC_MEDIA_INTERACTION || source === SRC_SELECTION) {
    var out2 = { source: source };
    if (typeof data.id === 'number') out2.id = data.id;
    if (typeof data.x === 'number') out2.x = data.x;
    if (typeof data.y === 'number') out2.y = data.y;
    if (typeof data.width === 'number') out2.width = data.width;
    if (typeof data.height === 'number') out2.height = data.height;
    if (typeof data.type === 'number') out2.type = data.type;
    if (Array.isArray(data.ranges)) out2.ranges = data.ranges;
    return out2;
  }

  if (source === SRC_INPUT) {
    // Input text NEVER rides an incremental event (the structural recorder
    // records {target, masked: true} only). Keep the id and checked state
    // for replay structure; rrweb's own maskAllInputs already masked the
    // text before emit, and this drops whatever remains.
    var inp = { source: SRC_INPUT };
    if (typeof data.id === 'number') inp.id = data.id;
    if (typeof data.isChecked === 'boolean') inp.isChecked = data.isChecked;
    if (data.userTriggered === true) inp.userTriggered = true;
    return inp;
  }

  // StyleSheetRule(8), CanvasMutation(9), Font(10), Log(11),
  // StyleDeclaration(13), AdoptedStyleSheet(15), CustomElement(16) and any
  // future source: dropped. Style and log payloads carry raw CSS/text we
  // have never recorded (style is a private tag in the structural policy);
  // the others are plugins this recorder never enables.
  return null;
}

function num(v) {
  return typeof v === 'number' && isFinite(v) ? v : 0;
}

function snapshotCtx(state) {
  var nodes = { count: 0, limited: function () { return false; } };
  return {
    get nodes() { return nodes.count; },
    set nodes(v) { nodes.count = v; },
    budget: unlimitedBudget(),
    privateIds: state.privateIds,
    baseURL: state.baseURL
  };
}

function unlimitedBudget() {
  return { limited: function () { return false; } };
}

function fullSnapshotCtx(state) {
  // A fresh snapshot resets the id space (rrweb re-mints ids from 1 at each
  // checkout), so the private-id set is rebuilt from this snapshot alone.
  state.privateIds = new Set();
  var count = 0;
  return {
    get nodes() { return count; },
    set nodes(v) { count = v; },
    budget: {
      limited: function () { return count > MAX_SNAPSHOT_NODES; }
    },
    privateIds: state.privateIds,
    baseURL: state.baseURL
  };
}

// sanitizeEventWith is the pure fold. state = { privateIds: Set<number>,
// baseURL?: string }; the returned state object is fresh when changed (the
// Set is mutated in place for the id adds — the fold is still a
// deterministic function of the event sequence, which is what the fixtures
// pin).
export function sanitizeEventWith(state, event) {
  if (!isPlainObject(event)) return { state: state, event: null };
  var type = event.type;

  if (type === EV_DOM_CONTENT_LOADED || type === EV_LOAD) {
    // rrweb emits both with data: {} — pure timestamp markers.
    return { state: state, event: { type: type, data: {}, timestamp: event.timestamp } };
  }

  if (type === EV_META) {
    var data = isPlainObject(event.data) ? event.data : {};
    var href = sanitizePageURL(data.href);
    var next = { privateIds: state.privateIds, baseURL: href || state.baseURL };
    return {
      state: next,
      event: {
        type: EV_META,
        data: { href: href, width: num(data.width), height: num(data.height) },
        timestamp: event.timestamp
      }
    };
  }

  if (type === EV_FULL_SNAPSHOT) {
    var fdata = isPlainObject(event.data) ? event.data : null;
    if (!fdata || !isPlainObject(fdata.node)) return { state: state, event: null };
    var ctx = fullSnapshotCtx(state);
    var doc = sanitizeDocumentNode(fdata.node, ctx);
    if (doc === null) return { state: state, event: null };
    var outData = { node: doc, initialOffset: {} };
    if (isPlainObject(fdata.initialOffset)) {
      var io = fdata.initialOffset;
      outData.initialOffset = {
        left: num(io.left), top: num(io.top)
      };
    }
    return {
      state: { privateIds: state.privateIds, baseURL: state.baseURL },
      event: { type: EV_FULL_SNAPSHOT, data: outData, timestamp: event.timestamp }
    };
  }

  if (type === EV_INCREMENTAL) {
    var idata = isPlainObject(event.data) ? event.data : null;
    if (!idata || typeof idata.source !== 'number') return { state: state, event: null };
    var sdata = sanitizeIncrementalData(idata, state);
    if (sdata === null) return { state: state, event: null };
    return {
      state: state,
      event: { type: EV_INCREMENTAL, data: sdata, timestamp: event.timestamp }
    };
  }

  // Custom(5) / Plugin(6) / Asset(7) / unknown future types: dropped. This
  // recorder never emits them, and an unknown payload is exactly what the
  // allowlist exists to refuse.
  return { state: state, event: null };
}

// createEventSanitizer drives the fold for one event stream (one recorder
// instance, or one player session). options.baseURL seeds relative-src
// resolution when no Meta event has been seen yet.
export function createEventSanitizer(options) {
  var state = { privateIds: new Set(), baseURL: (options && options.baseURL) || '' };
  return {
    sanitize: function (event) {
      var r = sanitizeEventWith(state, event);
      state = r.state;
      return r.event;
    }
  };
}

export const sanitizerPolicy = {
  CAPTURE_ATTRS: CAPTURE_ATTRS,
  PRIVATE_TAG: PRIVATE_TAG,
  MAX_TEXT_LENGTH: MAX_TEXT_LENGTH,
  MAX_SNAPSHOT_NODES: MAX_SNAPSHOT_NODES,
  MAX_SNAPSHOT_DEPTH: MAX_SNAPSHOT_DEPTH,
  sanitizePageURL: sanitizePageURL
};
