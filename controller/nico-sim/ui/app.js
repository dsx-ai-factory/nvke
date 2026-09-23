/**
 * SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

// Default org from seed data. Change if using a different seed.
var ORG_ID = 'novxwn552ssc';
var TOKEN = 'nico-sim-ui';
var REFRESH_MS = 5000;

var refreshTimer = null;
var followController = null;
var backfillController = null;
var consoleTerm = null;
var consoleFitAddon = null;
var consoleSearchAddon = null;
var diagCatalog = [];
var activeTab = 'console';
var instanceTypeNames = {};
var currentConsoleInstanceId = null;
var currentConsoleInstanceName = null;
var filesTreeState = null;
var filesNodeIndex = {};
var selectedFilesPath = '';
var filesPreviewRequestId = 0;
var currentView = 'instances';

// ── API ──────────────────────────────────────────────────────────

function apiHeaders() {
    return { 'Authorization': 'Bearer ' + TOKEN };
}

function fetchInstanceTypes() {
    return fetch('/v2/org/' + ORG_ID + '/forge/site?pageSize=200', {
        headers: apiHeaders()
    }).then(function(r) {
        if (!r.ok) return [];
        return r.json();
    }).then(function(sites) {
        var fetches = sites.map(function(site) {
            return fetch('/v2/org/' + ORG_ID + '/forge/instance/type?pageSize=200&siteId=' + site.id, {
                headers: apiHeaders()
            }).then(function(r) { return r.ok ? r.json() : []; });
        });
        return Promise.all(fetches);
    }).then(function(results) {
        results.forEach(function(types) {
            types.forEach(function(t) { instanceTypeNames[t.id] = t.name; });
        });
    }).catch(function() {});
}

function deleteInstance(instanceId) {
    return fetch('/v2/org/' + ORG_ID + '/forge/instance/' + instanceId, {
        method: 'DELETE',
        headers: apiHeaders()
    }).then(function(r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.json();
    });
}

function fetchInstances() {
    return fetch('/v2/org/' + ORG_ID + '/forge/instance?pageSize=200&orderBy=CREATED_ASC', {
        headers: apiHeaders()
    }).then(function(r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.json();
    });
}

function fetchFilesTree() {
    return fetch('/server/files-tree', {
        headers: apiHeaders()
    }).then(function(r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.json();
    });
}

function viewPath(viewName) {
    return viewName === 'files' ? '/ui/files' : '/ui/instances';
}

function viewFromLocation() {
    var path = window.location.pathname.replace(/\/+$/, '');
    if (path === '/ui/files') return 'files';
    if (path === '/ui/instances') return 'instances';
    return 'instances';
}

function switchView(viewName, updateURL) {
    currentView = viewName;
    document.querySelectorAll('.view-tab').forEach(function(el) {
        var active = el.getAttribute('data-view') === viewName;
        el.classList.toggle('active', active);
        el.setAttribute('aria-selected', active ? 'true' : 'false');
    });
    document.getElementById('instances-view').hidden = viewName !== 'instances';
    document.getElementById('files-view').hidden = viewName !== 'files';
    if (updateURL) {
        var targetPath = viewPath(viewName);
        if (window.location.pathname !== targetPath) {
            window.history.pushState({ view: viewName }, '', targetPath);
        }
    }
}

function reverseLines(s) {
    var lines = s.split('\n');
    lines.reverse();
    return lines.join('\n');
}

function streamConsoleLog(instanceId, el) {
    return fetch('/v2/org/' + ORG_ID + '/forge/instance/' + instanceId + '/console-log?order=newest', {
        headers: apiHeaders()
    }).then(function(r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        var reader = r.body.getReader();
        var decoder = new TextDecoder();
        var text = '';
        var lastRender = 0;
        var RENDER_INTERVAL = 200; // ms between DOM updates
        el.innerHTML = '';

        function render() {
            // Data arrives newest-first; reverse to display chronologically.
            // Scroll to bottom so the user sees recent output first.
            el.innerHTML = ansiToHtml(reverseLines(text));
            el.scrollTop = el.scrollHeight;
        }

        function read() {
            return reader.read().then(function(result) {
                if (result.done) {
                    render();
                    return;
                }
                text += decoder.decode(result.value, { stream: true });
                var now = Date.now();
                if (now - lastRender > RENDER_INTERVAL) {
                    lastRender = now;
                    render();
                }
                return read();
            });
        }
        return read();
    });
}

// ── Rendering ────────────────────────────────────────────────────

function statusBadgeClass(status) {
    var s = (status || '').toLowerCase();
    if (s === 'ready') return 'badge-ready';
    if (s === 'provisioning') return 'badge-provisioning';
    if (s === 'configuring') return 'badge-configuring';
    if (s === 'new') return 'badge-new';
    if (s === 'rebooting') return 'badge-rebooting';
    if (s === 'error') return 'badge-error';
    if (s === 'terminating') return 'badge-terminating';
    return '';
}

function getIP(instance) {
    var ifaces = instance.interfaces || [];
    for (var i = 0; i < ifaces.length; i++) {
        var ips = ifaces[i].ipAddresses || [];
        if (ips.length > 0) return ips[0];
    }
    return '\u2014';
}

function timeAgo(dateStr) {
    if (!dateStr) return '\u2014';
    var now = Date.now();
    var then = new Date(dateStr).getTime();
    var sec = Math.floor((now - then) / 1000);
    if (sec < 60) return sec + 's ago';
    var min = Math.floor(sec / 60);
    if (min < 60) return min + 'm ago';
    var hr = Math.floor(min / 60);
    if (hr < 24) return hr + 'h ago';
    var d = Math.floor(hr / 24);
    return d + 'd ago';
}

function boolIndicator(val) {
    return val ? '<span class="bool-yes">Yes</span>' : '<span class="bool-no">No</span>';
}

function escapeHtml(str) {
    var div = document.createElement('div');
    div.textContent = str;
    return div.innerHTML;
}

function buildRowCells(inst) {
    var status = inst.status || 'Unknown';
    var ip = getIP(inst);
    var ipxe = inst.ipxeScript || '';
    var isDHCP = ipxe !== '';
    return {
        name: '<span class="chevron">&#9656;</span> ' + escapeHtml(inst.name || inst.id),
        status: '<span class="badge ' + statusBadgeClass(status) + '">' + escapeHtml(status) + '</span>',
        ip: escapeHtml(ip),
        itype: escapeHtml(instanceTypeNames[inst.instanceTypeId] || inst.instanceTypeId || '\u2014'),
        dhcp: boolIndicator(isDHCP),
        phone: boolIndicator(inst.phoneHomeEnabled),
        created: timeAgo(inst.created),
    };
}

function detectLang(text) {
    if (/^#cloud-config/m.test(text) || /^[a-z_]+:/m.test(text)) return 'yaml';
    return 'shell';
}

function buildDetailHtml(inst) {
    var ipxe = inst.ipxeScript || '';
    var userData = inst.userData || '';
    var html = '<div class="detail-content">';
    if (ipxe) html += '<div class="detail-field"><label>iPXE Script</label><pre><code class="language-shell">' + escapeHtml(ipxe) + '</code></pre></div>';
    if (userData) {
        html += '<div class="detail-field"><label>User Data</label><pre><code class="language-' + detectLang(userData) + '">' + escapeHtml(userData) + '</code></pre></div>';
    } else {
        html += '<div class="detail-field"><label>User Data</label><div class="detail-empty">Empty</div></div>';
    }
    return html + '</div>';
}

function updateRow(tr, inst) {
    var cells = buildRowCells(inst);
    var tds = tr.children;
    // Preserve chevron open/closed state
    var chevron = tds[0].querySelector('.chevron');
    var wasOpen = chevron && chevron.classList.contains('open');
    tds[0].innerHTML = cells.name;
    if (wasOpen) {
        var newChevron = tds[0].querySelector('.chevron');
        if (newChevron) newChevron.classList.add('open');
    }
    tds[1].innerHTML = cells.status;
    tds[2].innerHTML = cells.ip;
    tds[3].innerHTML = cells.itype;
    tds[4].innerHTML = cells.dhcp;
    tds[5].innerHTML = cells.phone;
    tds[6].innerHTML = cells.created;
    tds[0].title = inst.id;
    tds[6].title = inst.created || '';
}

function createInstanceRow(inst) {
    var cells = buildRowCells(inst);
    var tr = document.createElement('tr');
    tr.className = 'expandable';
    tr.setAttribute('data-instance-id', inst.id);
    tr.innerHTML =
        '<td class="col-name" title="' + escapeHtml(inst.id) + '">' + cells.name + '</td>' +
        '<td>' + cells.status + '</td>' +
        '<td class="col-ip">' + cells.ip + '</td>' +
        '<td class="col-id">' + cells.itype + '</td>' +
        '<td>' + cells.dhcp + '</td>' +
        '<td>' + cells.phone + '</td>' +
        '<td title="' + escapeHtml(inst.created || '') + '">' + cells.created + '</td>' +
        '<td><button class="btn btn-console">Inspect</button> <button class="btn btn-delete">Delete</button></td>';
    tr.querySelector('.btn-console').addEventListener('click', function(e) {
        e.stopPropagation();
        openConsoleLog(inst.id, inst.name || inst.id);
    });
    tr.querySelector('.btn-delete').addEventListener('click', function(e) {
        e.stopPropagation();
        if (confirm('Delete instance "' + (inst.name || inst.id) + '"?')) {
            deleteInstance(inst.id).then(function() {
                loadInstances();
            });
        }
    });
    bindRowToggle(tr);

    return { tr: tr, detail: createInstanceDetailRow(inst, 8) };
}

function createInstanceDetailRow(inst, columnCount) {
    var detailHtml = buildDetailHtml(inst);
    if (!detailHtml) return null;
    var detail = document.createElement('tr');
    detail.className = 'detail-row';
    detail.hidden = true;
    var td = document.createElement('td');
    td.colSpan = columnCount;
    td.innerHTML = detailHtml;
    detail.appendChild(td);
    detail.querySelectorAll('pre code').forEach(function(block) { hljs.highlightElement(block); });
    return detail;
}

function bindRowToggle(tr) {
    tr.addEventListener('click', function() {
        var detail = this.nextElementSibling;
        if (!detail || !detail.classList.contains('detail-row')) return;
        detail.hidden = !detail.hidden;
        var chevron = this.querySelector('.chevron');
        if (chevron) chevron.classList.toggle('open', !detail.hidden);
    });
}

function renderVMInstances(instances) {
    var tbody = document.getElementById('vm-instance-tbody');
    var countEl = document.getElementById('vm-instance-count');
    countEl.textContent = instances.length;

    if (instances.length === 0) {
        tbody.innerHTML = '<tr><td colspan="8" class="empty-state">No VMs</td></tr>';
        return;
    }

    // Index existing rows by instance ID
    var existing = {};
    tbody.querySelectorAll('tr[data-instance-id]').forEach(function(row) {
        existing[row.getAttribute('data-instance-id')] = row;
    });

    // Track new instance IDs for removal pass
    var seen = {};

    for (var i = 0; i < instances.length; i++) {
        var inst = instances[i];
        seen[inst.id] = true;
        var row = existing[inst.id];
        if (row) {
            // Update cells in place — detail row visibility is untouched
            updateRow(row, inst);
        } else {
            // New instance — create and append
            var created = createInstanceRow(inst);
            tbody.appendChild(created.tr);
            if (created.detail) tbody.appendChild(created.detail);
        }
    }

    // Remove rows for instances that no longer exist
    for (var id in existing) {
        if (!seen[id]) {
            var row = existing[id];
            var detail = row.nextElementSibling;
            if (detail && detail.classList.contains('detail-row')) detail.remove();
            row.remove();
        }
    }

    // Clear the "Loading..." placeholder if still present
    var empty = tbody.querySelector('.empty-state');
    if (empty) empty.closest('tr').remove();
}

function createContainerInstanceRow(inst) {
    var cells = buildRowCells(inst);
    var tr = document.createElement('tr');
    tr.className = 'expandable';
    tr.setAttribute('data-instance-id', inst.id);
    tr.innerHTML =
        '<td class="col-name" title="' + escapeHtml(inst.id) + '">' + cells.name + '</td>' +
        '<td>' + cells.status + '</td>' +
        '<td class="col-ip">' + cells.ip + '</td>' +
        '<td class="col-id">' + cells.itype + '</td>' +
        '<td>' + cells.dhcp + '</td>' +
        '<td>' + cells.phone + '</td>' +
        '<td title="' + escapeHtml(inst.created || '') + '">' + cells.created + '</td>' +
        '<td><button class="btn btn-delete">Delete</button></td>';
    tr.querySelector('.btn-delete').addEventListener('click', function(e) {
        e.stopPropagation();
        if (confirm('Delete container "' + (inst.name || inst.id) + '"?')) {
            deleteInstance(inst.id).then(function() {
                loadInstances();
            });
        }
    });
    bindRowToggle(tr);
    return { tr: tr, detail: createInstanceDetailRow(inst, 8) };
}

function renderContainerInstances(instances) {
    var tbody = document.getElementById('container-instance-tbody');
    document.getElementById('container-instance-count').textContent = instances.length;
    if (instances.length === 0) {
        tbody.innerHTML = '<tr><td colspan="8" class="empty-state">No containers</td></tr>';
        return;
    }

    var existing = {};
    tbody.querySelectorAll('tr[data-instance-id]').forEach(function(row) {
        existing[row.getAttribute('data-instance-id')] = row;
    });

    var seen = {};
    instances.forEach(function(inst) {
        seen[inst.id] = true;
        var row = existing[inst.id];
        if (row) {
            updateRow(row, inst);
            return;
        }
        var created = createContainerInstanceRow(inst);
        tbody.appendChild(created.tr);
        if (created.detail) tbody.appendChild(created.detail);
    });

    for (var id in existing) {
        if (seen[id]) continue;
        var row = existing[id];
        var detail = row.nextElementSibling;
        if (detail && detail.classList.contains('detail-row')) detail.remove();
        row.remove();
    }

    var empty = tbody.querySelector('.empty-state');
    if (empty) empty.closest('tr').remove();
}

function renderInstances(instances) {
    var vms = [];
    var containers = [];
    instances.forEach(function(inst) {
        if ((inst.labels || {})['nico-sim.nvidia.com/backend'] === 'container') {
            containers.push(inst);
        } else {
            vms.push(inst);
        }
    });
    renderVMInstances(vms);
    renderContainerInstances(containers);
}

function countFileNodes(node) {
    if (!node) return 0;
    if (node.type === 'file') return 1;
    var total = 0;
    var children = node.children || [];
    for (var i = 0; i < children.length; i++) {
        total += countFileNodes(children[i]);
    }
    return total;
}

function countChildEntries(node) {
    return (node && node.children ? node.children.length : 0);
}

function escapeAttribute(str) {
    return escapeHtml(str).replace(/"/g, '&quot;');
}

function formatBytes(bytes) {
    if (!bytes && bytes !== 0) return '\u2014';
    if (bytes === 0) return '0 B';
    var units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
    var value = bytes;
    var unit = 0;
    while (value >= 1024 && unit < units.length - 1) {
        value /= 1024;
        unit++;
    }
    var rounded = value >= 10 || unit === 0 ? Math.round(value) : Math.round(value * 10) / 10;
    return rounded + ' ' + units[unit];
}

function formatTimestamp(dateStr) {
    if (!dateStr) return '\u2014';
    var d = new Date(dateStr);
    if (isNaN(d.getTime())) return dateStr;
    return d.toLocaleString();
}

function buildServedFileURL(relPath) {
    if (!relPath) return '/files/';
    return '/files/' + relPath.split('/').map(encodeURIComponent).join('/');
}

function detectFileSyntax(name) {
    var lower = (name || '').toLowerCase();
    if (lower.endsWith('.yaml') || lower.endsWith('.yml')) return 'yaml';
    if (lower.endsWith('.json')) return 'json';
    if (lower.endsWith('.sh') || lower.endsWith('.bash') || lower.endsWith('.env')) return 'shell';
    if (lower.endsWith('.service') || lower.endsWith('.conf') || lower.endsWith('.cfg') || lower.endsWith('.ini') || lower.endsWith('.log') || lower.endsWith('.txt') || lower.endsWith('.md') || lower.endsWith('.toml')) return 'plaintext';
    return '';
}

function isPreviewableFile(node) {
    if (!node || node.type !== 'file') return false;
    if (node.size > 262144) return false;
    return detectFileSyntax(node.name) !== '';
}

function indexFilesNode(node, index) {
    index[node.path] = node;
    var children = node.children || [];
    for (var i = 0; i < children.length; i++) {
        indexFilesNode(children[i], index);
    }
}

function applySelectedFilesPath() {
    document.querySelectorAll('#files-tree .file-tree-entry').forEach(function(el) {
        el.classList.toggle('selected', el.getAttribute('data-path') === selectedFilesPath);
    });
}

function selectFilesPath(path) {
    selectedFilesPath = path;
    applySelectedFilesPath();
    renderFilesDetails();
}

function createFileTreeEntry(node, metaText) {
    var entry = document.createElement('button');
    entry.type = 'button';
    entry.className = 'file-tree-entry';
    entry.setAttribute('data-path', node.path);
    entry.innerHTML =
        '<span class="file-tree-name">' + escapeHtml(node.name) + '</span>' +
        '<span class="file-tree-meta">' + escapeHtml(metaText) + '</span>';
    entry.addEventListener('click', function() {
        selectFilesPath(node.path);
    });
    return entry;
}

function createFileTreeNode(node, isRoot, isLastSibling) {
    var hasChildren = node.type === 'dir' && node.children && node.children.length > 0;
    var wrapper = document.createElement('div');
    wrapper.className =
        'file-tree-node' +
        (isRoot ? ' root' : '') +
        (isLastSibling ? ' last-sibling' : '') +
        (node.type === 'dir' ? ' is-dir' : ' is-file') +
        (hasChildren ? ' has-children' : ' is-leaf');

    if (node.type === 'file') {
        wrapper.appendChild(createFileTreeEntry(node, formatBytes(node.size)));
        return wrapper;
    }

    wrapper.appendChild(createFileTreeEntry(node, String(countChildEntries(node))));

    var childrenWrap = document.createElement('div');
    childrenWrap.className = 'file-tree-children';

    var children = node.children || [];
    for (var i = 0; i < children.length; i++) {
        childrenWrap.appendChild(createFileTreeNode(children[i], false, i === children.length - 1));
    }
    wrapper.appendChild(childrenWrap);

    return wrapper;
}

function renderFilesTree() {
    var treeEl = document.getElementById('files-tree');
    treeEl.innerHTML = '';

    if (!filesTreeState) {
        treeEl.innerHTML = '<div class="files-tree-empty">Files tree unavailable.</div>';
        return;
    }

    if (!filesTreeState.exists) {
        treeEl.innerHTML = '<div class="files-tree-empty">Configured files root does not exist yet.</div>';
        return;
    }

    treeEl.appendChild(createFileTreeNode(filesTreeState.root, true, true));
    applySelectedFilesPath();
}

function renderFilesDetails() {
    var detailsEl = document.getElementById('files-details');
    var node = filesNodeIndex[selectedFilesPath];
    filesPreviewRequestId++;

    if (!filesTreeState) {
        detailsEl.innerHTML = '<div class="empty-state">Files tree unavailable.</div>';
        return;
    }

    if (!node) {
        detailsEl.innerHTML = '<div class="empty-state">Select a file or folder</div>';
        return;
    }

    var relPath = node.path || '.';
    var servedURL = node.type === 'file' ? buildServedFileURL(node.path) : '\u2014';
    var childCount = node.type === 'dir' ? String(countChildEntries(node)) : '\u2014';
    var typeClass = node.type === 'dir' ? 'value-type-dir' : 'value-type-file';
    var sizeClass = node.type === 'file' ? 'value-number' : 'value-muted';
    var childrenClass = node.type === 'dir' ? 'value-number' : 'value-muted';
    var urlMarkup = node.type === 'file'
        ? '<a href="' + escapeAttribute(servedURL) + '" target="_blank" rel="noopener noreferrer">' + escapeHtml(servedURL) + '</a>'
        : '<code class="value-muted">\u2014</code>';
    detailsEl.innerHTML =
        '<div class="files-details-header">' +
            '<h3>' + escapeHtml(node.name) + '</h3>' +
            '<div class="files-details-subtitle">' + escapeHtml(relPath) + '</div>' +
        '</div>' +
        '<div class="files-details-grid">' +
            '<div class="files-detail-field"><label>Type</label><code class="' + typeClass + '">' + escapeHtml(node.type === 'dir' ? 'Directory' : 'File') + '</code></div>' +
            '<div class="files-detail-field"><label>Modified</label><code>' + escapeHtml(formatTimestamp(node.modTime)) + '</code></div>' +
            '<div class="files-detail-field"><label>Size</label><code class="' + sizeClass + '">' + escapeHtml(node.type === 'file' ? formatBytes(node.size) : '\u2014') + '</code></div>' +
            '<div class="files-detail-field"><label>Children</label><code class="' + childrenClass + '">' + escapeHtml(childCount) + '</code></div>' +
            '<div class="files-detail-field"><label>Server Root</label><code class="value-path">' + escapeHtml(filesTreeState.rootPath || '\u2014') + '</code></div>' +
            '<div class="files-detail-field"><label>Served URL</label>' + urlMarkup + '</div>' +
        '</div>';

    if (!isPreviewableFile(node)) {
        return;
    }

    var previewId = filesPreviewRequestId;
    var preview = document.createElement('div');
    preview.className = 'files-preview';
    preview.innerHTML =
        '<label>Preview</label>' +
        '<pre><code id="files-preview-code" class="' + (detectFileSyntax(node.name) ? 'language-' + detectFileSyntax(node.name) : '') + '">Loading...</code></pre>';
    detailsEl.appendChild(preview);

    fetch(buildServedFileURL(node.path)).then(function(r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.text();
    }).then(function(text) {
        if (previewId !== filesPreviewRequestId) return;
        var codeEl = document.getElementById('files-preview-code');
        if (!codeEl) return;
        codeEl.textContent = text || '(empty)';
        hljs.highlightElement(codeEl);
    }).catch(function(err) {
        if (previewId !== filesPreviewRequestId) return;
        var codeEl = document.getElementById('files-preview-code');
        if (codeEl) codeEl.textContent = 'Error: ' + err.message;
    });
}

function renderFilesBrowser(data) {
    filesTreeState = data;
    filesNodeIndex = {};

    var rootPathEl = document.getElementById('files-root-path');
    var countEl = document.getElementById('files-count');

    rootPathEl.textContent = 'Configured Root: ' + (data.rootPath || 'Not configured');
    if (data.root) {
        indexFilesNode(data.root, filesNodeIndex);
    }
    countEl.textContent = data.exists && data.root ? String(countFileNodes(data.root)) : '0';

    if (!Object.prototype.hasOwnProperty.call(filesNodeIndex, selectedFilesPath)) {
        selectedFilesPath = '';
    }

    renderFilesTree();
    renderFilesDetails();
}

function loadFilesTree() {
    var treeEl = document.getElementById('files-tree');
    var detailsEl = document.getElementById('files-details');
    treeEl.innerHTML = '<div class="empty-state">Loading...</div>';
    detailsEl.innerHTML = '<div class="empty-state">Loading...</div>';

    fetchFilesTree().then(function(data) {
        renderFilesBrowser(data);
    }).catch(function(err) {
        document.getElementById('files-root-path').textContent = 'Configured Root: Unavailable';
        document.getElementById('files-count').textContent = '0';
        treeEl.innerHTML = '<div class="empty-state">Error loading files tree: ' + escapeHtml(err.message) + '</div>';
        detailsEl.innerHTML = '<div class="empty-state">Unable to load files tree.</div>';
    });
}

// ── Refresh loop ─────────────────────────────────────────────────

function loadInstances() {
    var dot = document.getElementById('status-indicator');
    fetchInstances().then(function(data) {
        dot.className = 'status-dot';
        renderInstances(data);
    }).catch(function() {
        dot.className = 'status-dot error';
    });
}

function startRefresh() {
    if (refreshTimer) clearInterval(refreshTimer);
    refreshTimer = setInterval(loadInstances, REFRESH_MS);
}

function stopRefresh() {
    if (refreshTimer) { clearInterval(refreshTimer); refreshTimer = null; }
}

// ── ANSI → HTML ──────────────────────────────────────────────────

// Replace sequences that destroy scrollback with newlines for readability:
// ESC[2J / ESC[3J (erase display/scrollback), ESC[=3h (set display mode),
// ESC[H / ESC[1;1H (cursor home), ESC[<n>;<n>H (absolute cursor positioning).
var screenControlRe = /\x1b\[[23]J|\x1b\[=\d*h|\x1b\[\d*;\d*[Hf]|\x1b\[H/g;

function filterForScrollback(text) {
    return text.replace(screenControlRe, '\n');
}

var ANSI_COLORS = [
    '#000', '#c00', '#0a0', '#a50', '#00a', '#a0a', '#0aa', '#aaa', // 0-7
    '#555', '#f55', '#5f5', '#ff5', '#55f', '#f5f', '#5ff', '#fff', // 8-15 (bright)
];

function ansiToHtml(text) {
    var out = '';
    var spans = 0;
    var i = 0;
    while (i < text.length) {
        // Match ESC[ ... m (SGR sequence)
        if (text[i] === '\x1b' && text[i + 1] === '[') {
            var end = text.indexOf('m', i + 2);
            if (end !== -1 && end - i < 20) {
                var codes = text.substring(i + 2, end).split(';');
                var styles = ansiCodesToStyle(codes);
                if (styles === null) {
                    // reset: close all open spans
                    while (spans > 0) { out += '</span>'; spans--; }
                } else if (styles !== '') {
                    out += '<span style="' + styles + '">';
                    spans++;
                }
                i = end + 1;
                continue;
            }
        }
        // HTML-escape regular characters
        var ch = text[i];
        if (ch === '<') out += '&lt;';
        else if (ch === '>') out += '&gt;';
        else if (ch === '&') out += '&amp;';
        else out += ch;
        i++;
    }
    while (spans > 0) { out += '</span>'; spans--; }
    return out;
}

function ansiCodesToStyle(codes) {
    // Returns CSS string, null for reset, or '' for no-op
    var fg = '', bg = '', bold = false, dim = false;
    var isReset = false;
    for (var i = 0; i < codes.length; i++) {
        var c = parseInt(codes[i], 10) || 0;
        if (c === 0) { isReset = true; continue; }
        if (c === 1) bold = true;
        else if (c === 2) dim = true;
        else if (c >= 30 && c <= 37) fg = ANSI_COLORS[c - 30];
        else if (c >= 40 && c <= 47) bg = ANSI_COLORS[c - 40];
        else if (c >= 90 && c <= 97) fg = ANSI_COLORS[c - 90 + 8];
        else if (c >= 100 && c <= 107) bg = ANSI_COLORS[c - 100 + 8];
    }
    if (isReset && !fg && !bg && !bold && !dim) return null;
    var s = '';
    if (fg) s += 'color:' + fg + ';';
    if (bg) s += 'background:' + bg + ';';
    if (bold) s += 'font-weight:bold;';
    if (dim) s += 'opacity:0.7;';
    return s;
}

// ── Log grammar + per-line highlighting ──────────────────────────

hljs.registerLanguage('log', function() {
    // Color mapping (atom-one-dark):
    //   comment=#5c6370  number=#d19a66  string=#98c379  keyword=#c678dd
    //   built_in=#56b6c2  title=#61afef  type=#e5c07b  literal=#d19a66
    return {
        name: 'Log',
        case_insensitive: false,
        contains: [
            // Log levels — custom colors via CSS
            { className: 'log-error', match: /\b(ERROR|FATAL|CRITICAL|EMERG|ALERT|PANIC)\b/ },
            { className: 'log-warn',  match: /\b(WARN|WARNING)\b/ },
            { className: 'log-info',  match: /\b(INFO|NOTICE)\b/ },
            { className: 'log-debug', match: /\b(DEBUG|TRACE)\b/ },
            // klog levels (kubelet): E0408=error, W0408=warn, I0408=info, D0408=debug
            { className: 'log-error', match: /\bE\d{4}\b/ },
            { className: 'log-warn',  match: /\bW\d{4}\b/ },
            { className: 'log-info',  match: /\bI\d{4}\b/ },
            { className: 'log-debug', match: /\bD\d{4}\b/ },
            // Timestamps — muted (grey)
            { className: 'comment',   match: /\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}[.\d]*(Z|[+-]\d{2}:?\d{2})?/ },
            { className: 'comment',   match: /[A-Z][a-z]{2}\s+\d{1,2}\s+\d{2}:\d{2}:\d{2}/ },
            // Quoted strings — green
            { className: 'string',    begin: /"/, end: /"/, contains: [{ begin: /\\./ }] },
            // Key=value keys — cyan
            { className: 'built_in',  match: /[\w.-]+(?==)/ },
            // JSON-style "key": — cyan
            { className: 'built_in',  match: /"[\w.-]+"(?=\s*:)/ },
            // Boolean/null literals — purple
            { className: 'keyword',   match: /\b(true|false|null|nil)\b/ },
            // IP addresses — blue
            { className: 'title',     match: /\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(:\d+)?\b/ },
            // UUIDs — blue
            { className: 'title',     match: /\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b/i },
            // File paths — yellow
            { className: 'type',      match: /\/[\w./-]+/ },
            // Process/unit names in brackets — yellow
            { className: 'type',      match: /\w+\[\d+\]:/ },
            // Numbers — orange
            { className: 'number',    match: /\b\d+(\.\d+)?\b/ },
        ]
    };
});

function highlightLogLine(line) {
    var trimmed = line.trimStart();
    if (trimmed.charAt(0) === '{' && trimmed.charAt(trimmed.length - 1) === '}') {
        try { return hljs.highlight(line, { language: 'json' }).value; } catch (e) {}
    }
    return hljs.highlight(line, { language: 'log' }).value;
}

function appendLogLine(preEl, line) {
    var div = document.createElement('div');
    div.className = 'log-line';
    div.innerHTML = highlightLogLine(line);
    preEl.appendChild(div);
}

// ── Console Log Modal ────────────────────────────────────────────

function setConsoleLoading(loading) {
    var spinner = document.getElementById('console-spinner');
    spinner.hidden = !loading;
}

function initTerminal() {
    var container = document.getElementById('console-output');
    if (consoleTerm) { consoleTerm.dispose(); consoleTerm = null; }
    container.innerHTML = '';
    consoleTerm = new Terminal({
        theme: {
            background: '#121212',
            foreground: '#e0e0e0',
            cursor: '#76b900',
            selectionBackground: 'rgba(118, 185, 0, 0.3)',
        },
        scrollback: 50000,
        fontSize: 13,
        fontFamily: "'SF Mono', 'Fira Code', 'Cascadia Code', monospace",
        disableStdin: true,
        convertEol: true,
        allowProposedApi: true,
    });
    consoleFitAddon = new FitAddon.FitAddon();
    consoleSearchAddon = new SearchAddon.SearchAddon();
    consoleTerm.loadAddon(consoleFitAddon);
    consoleTerm.loadAddon(consoleSearchAddon);
    consoleTerm.open(container);
    consoleFitAddon.fit();
}

function openConsoleSearch() {
    var bar = document.getElementById('console-search');
    bar.hidden = false;
    var input = document.getElementById('console-search-input');
    input.value = '';
    input.focus();
}

function closeConsoleSearch() {
    document.getElementById('console-search').hidden = true;
    if (activeTab === 'console' && consoleSearchAddon) consoleSearchAddon.clearDecorations();
    if (activeTab === 'console' && consoleTerm) consoleTerm.focus();
    clearPreSearch();
}

var SEARCH_DECORATIONS = {
    matchBackground: '#3d5c00',
    matchBorder: '#76b900',
    matchOverviewRuler: '#76b900',
    activeMatchBackground: '#4a7a00',
    activeMatchBorder: '#8dd419',
    activeMatchColorOverviewRuler: '#8dd419',
};

// ── Pre-element text search ──────────────────────────────────────

var preSearchMarks = [];
var preSearchIndex = -1;
var preSearchQuery = '';

function getActivePreElement() {
    if (activeTab === 'console') return null;
    var log = document.getElementById('log-output');
    var file = document.getElementById('file-output');
    if (!log.hidden) return log;
    if (!file.hidden) return file;
    return null;
}

function clearPreSearch() {
    // Unwrap all <mark> elements back to text nodes.
    for (var i = 0; i < preSearchMarks.length; i++) {
        var mark = preSearchMarks[i];
        if (mark.parentNode) {
            var text = document.createTextNode(mark.textContent);
            mark.parentNode.replaceChild(text, mark);
        }
    }
    // Normalize merges adjacent text nodes.
    var pre = getActivePreElement();
    if (pre) pre.normalize();
    preSearchMarks = [];
    preSearchIndex = -1;
    preSearchQuery = '';
}

function applyPreSearch(query) {
    clearPreSearch();
    preSearchQuery = query;
    var pre = getActivePreElement();
    if (!pre || !query) return;

    // Find all ranges first.
    var ranges = [];
    var lower = query.toLowerCase();
    var walker = document.createTreeWalker(pre, NodeFilter.SHOW_TEXT);
    var node;
    while ((node = walker.nextNode()) !== null) {
        var text = node.textContent.toLowerCase();
        var pos = 0;
        while ((pos = text.indexOf(lower, pos)) !== -1) {
            var r = document.createRange();
            r.setStart(node, pos);
            r.setEnd(node, pos + query.length);
            ranges.push(r);
            pos += query.length;
        }
    }

    // Wrap matches from last to first so earlier ranges stay valid.
    preSearchMarks = new Array(ranges.length);
    for (var i = ranges.length - 1; i >= 0; i--) {
        try {
            var mark = document.createElement('mark');
            mark.className = 'search-match';
            ranges[i].surroundContents(mark);
            preSearchMarks[i] = mark;
        } catch (e) {
            preSearchMarks[i] = null;
        }
    }
    // Remove nulls from failed surroundContents.
    preSearchMarks = preSearchMarks.filter(function(m) { return m !== null; });
}

function navigatePreSearch(forward) {
    if (preSearchMarks.length === 0) return;

    // Remove active class from current.
    if (preSearchIndex >= 0 && preSearchIndex < preSearchMarks.length) {
        preSearchMarks[preSearchIndex].className = 'search-match';
    }

    if (forward) {
        preSearchIndex = (preSearchIndex + 1) % preSearchMarks.length;
    } else {
        preSearchIndex = (preSearchIndex - 1 + preSearchMarks.length) % preSearchMarks.length;
    }

    preSearchMarks[preSearchIndex].className = 'search-match active';
    preSearchMarks[preSearchIndex].scrollIntoView({ block: 'center' });
}

// ── Unified search dispatch ──────────────────────────────────────

function consoleSearchNext() {
    var q = document.getElementById('console-search-input').value;
    if (!q) return;
    if (activeTab === 'console' && consoleSearchAddon) {
        consoleSearchAddon.findNext(q, { decorations: SEARCH_DECORATIONS });
    } else {
        if (q !== preSearchQuery) applyPreSearch(q);
        navigatePreSearch(true);
    }
}

function consoleSearchPrev() {
    var q = document.getElementById('console-search-input').value;
    if (!q) return;
    if (activeTab === 'console' && consoleSearchAddon) {
        consoleSearchAddon.findPrevious(q, { decorations: SEARCH_DECORATIONS });
    } else {
        if (q !== preSearchQuery) applyPreSearch(q);
        navigatePreSearch(false);
    }
}

function openConsoleLog(instanceId, name) {
    currentConsoleInstanceId = instanceId;
    currentConsoleInstanceName = name;
    activeTab = '';
    document.getElementById('console-title').textContent = name;
    document.getElementById('console-follow').checked = false;
    document.getElementById('console-source').value = 'auto';
    document.getElementById('console-modal').hidden = false;
    document.getElementById('console-output').hidden = true;
    document.getElementById('file-output').hidden = true;
    document.getElementById('console-follow').parentElement.style.display = 'none';

    fetchDiagCatalog(instanceId).then(function(items) {
        diagCatalog = items;
        renderTabs();
    });
}

function refreshConsoleLog() {
    if (!currentConsoleInstanceId) return;
    initTerminal();
    startFollow();
}

function startFollow() {
    stopFollow();
    if (!currentConsoleInstanceId || !consoleTerm) return;
    setConsoleLoading(true);
    consoleTerm.clear();

    var liveChunks = []; // raw strings for backfill replay
    var backfillDone = false;

    followController = new AbortController();
    backfillController = new AbortController();
    var instanceId = currentConsoleInstanceId;
    var baseUrl = '/v2/org/' + ORG_ID + '/forge/instance/' + instanceId + '/console-log';
    var source = document.getElementById('console-source').value || 'auto';
    var sourceParam = '&source=' + encodeURIComponent(source);

    fetch(baseUrl + '?follow=true&tail=500' + sourceParam, {
        headers: apiHeaders(),
        signal: followController.signal
    }).then(function(r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);

        var initialSize = parseInt(r.headers.get('X-Initial-Size') || '0', 10);
        var bytesReceived = 0;
        var initialDone = (initialSize <= 0);

        // Background backfill.
        var tailOffset = r.headers.get('X-Tail-Offset');
        if (tailOffset && parseInt(tailOffset, 10) > 0) {
            fetch(baseUrl + '?end=' + tailOffset + sourceParam, {
                headers: apiHeaders(),
                signal: backfillController.signal
            }).then(function(br) {
                return br.ok ? br.text() : '';
            }).then(function(text) {
                if (!text) { setConsoleLoading(false); return; }
                backfillDone = true;

                // Build the full content in a hidden terminal to avoid flicker.
                var container = document.getElementById('console-output');
                var hiddenDiv = document.createElement('div');
                hiddenDiv.style.cssText = 'position:absolute;visibility:hidden;width:' + container.offsetWidth + 'px;height:' + container.offsetHeight + 'px;';
                container.appendChild(hiddenDiv);

                var newTerm = new Terminal(consoleTerm.options);
                var newFit = new FitAddon.FitAddon();
                var newSearch = new SearchAddon.SearchAddon();
                newTerm.loadAddon(newFit);
                newTerm.loadAddon(newSearch);
                newTerm.open(hiddenDiv);
                newFit.fit();

                // Write backfill + accumulated live chunks.
                var allData = filterForScrollback(text);
                for (var i = 0; i < liveChunks.length; i++) {
                    allData += liveChunks[i];
                }
                newTerm.write(allData, function() {
                    // Swap: dispose old terminal, reveal new one in place.
                    consoleTerm.dispose();
                    hiddenDiv.style.cssText = 'height:100%;';
                    consoleTerm = newTerm;
                    consoleFitAddon = newFit;
                    consoleSearchAddon = newSearch;
                    consoleFitAddon.fit();
                    consoleTerm.scrollToBottom();
                    liveChunks = [];
                    setConsoleLoading(false);
                });
            }).catch(function() { setConsoleLoading(false); });
        } else {
            setConsoleLoading(false);
        }

        var reader = r.body.getReader();
        var decoder = new TextDecoder();

        function read() {
            return reader.read().then(function(result) {
                if (result.done) return;
                bytesReceived += result.value.length;
                var chunk = filterForScrollback(decoder.decode(result.value, { stream: true }));

                if (!backfillDone) liveChunks.push(chunk);

                if (!initialDone) {
                    if (bytesReceived >= initialSize) {
                        initialDone = true;
                        for (var i = 0; i < liveChunks.length; i++) {
                            consoleTerm.write(liveChunks[i]);
                        }
                        consoleTerm.scrollToBottom();
                    }
                } else {
                    consoleTerm.write(chunk);
                    consoleTerm.scrollToBottom();
                }
                return read();
            });
        }
        return read();
    }).catch(function(err) {
        setConsoleLoading(false);
        if (err.name !== 'AbortError' && consoleTerm) {
            consoleTerm.write('\r\n\x1b[31mError: ' + (err.message || 'unknown') + '\x1b[0m\r\n');
        }
    });
}

function stopFollow() {
    if (followController) {
        followController.abort();
        followController = null;
    }
}

function stopBackfill() {
    if (backfillController) {
        backfillController.abort();
        backfillController = null;
    }
    setConsoleLoading(false);
}

function closeConsoleLog() {
    stopFollow();
    stopBackfill();
    if (consoleTerm) { consoleTerm.dispose(); consoleTerm = null; }
    document.getElementById('console-output').innerHTML = '';
    document.getElementById('log-output').innerHTML = '';
    document.getElementById('file-output').innerHTML = '';
    document.getElementById('diag-tabs').innerHTML = '';
    document.getElementById('console-search').hidden = true;
    document.getElementById('console-follow').checked = false;
    document.getElementById('console-modal').hidden = true;
    diagCatalog = [];
    activeTab = '';
    currentConsoleInstanceId = null;
    currentConsoleInstanceName = null;
}

// ── Diagnostics tabs ─────────────────────────────────────────────

function fetchDiagCatalog(instanceId) {
    return fetch('/v2/org/' + ORG_ID + '/forge/instance/' + instanceId + '/diagnostics', {
        headers: apiHeaders()
    }).then(function(r) { return r.ok ? r.json() : []; })
      .catch(function() { return []; });
}

function renderTabs() {
    var container = document.getElementById('diag-tabs');
    var html = '<button class="diag-tab' + (activeTab === 'console' ? ' active' : '') + '" onclick="switchTab(\'console\')">Console</button>';
    for (var i = 0; i < diagCatalog.length; i++) {
        var item = diagCatalog[i];
        html += '<button class="diag-tab' + (activeTab === item.id ? ' active' : '') + '" onclick="switchTab(\'' + item.id + '\')">' + escapeHtml(item.name) + '</button>';
    }
    container.innerHTML = html;
}

function switchTab(tabId) {
    activeTab = tabId;
    renderTabs();
    stopFollow();
    stopBackfill();

    var consoleEl = document.getElementById('console-output');
    var logEl = document.getElementById('log-output');
    var fileEl = document.getElementById('file-output');
    var followToggle = document.getElementById('console-follow').parentElement;
    var searchBar = document.getElementById('console-search');
    searchBar.hidden = true;

    // Hide all output panels.
    consoleEl.hidden = true;
    logEl.hidden = true;
    fileEl.hidden = true;
    if (consoleTerm) { consoleTerm.dispose(); consoleTerm = null; }

    if (tabId === 'console') {
        consoleEl.hidden = false;
        followToggle.style.display = '';
        initTerminal();
        document.getElementById('console-follow').checked = true;
        startFollow();
    } else {
        var item = null;
        for (var i = 0; i < diagCatalog.length; i++) {
            if (diagCatalog[i].id === tabId) { item = diagCatalog[i]; break; }
        }
        if (!item) return;

        if (item.type === 'file') {
            fileEl.hidden = false;
            followToggle.style.display = 'none';
            loadDiagFile(item);
        } else {
            logEl.hidden = false;
            followToggle.style.display = item.follow ? '' : 'none';
            if (item.follow) {
                document.getElementById('console-follow').checked = true;
                startDiagFollow(item);
            } else {
                document.getElementById('console-follow').checked = false;
                loadDiagLog(item);
            }
        }
    }
}

function loadDiagFile(item) {
    var preEl = document.getElementById('file-output');
    // Create a fresh <code> element so highlight.js doesn't skip it.
    preEl.innerHTML = '';
    var codeEl = document.createElement('code');
    codeEl.id = 'file-output-code';
    codeEl.className = item.syntax ? 'language-' + item.syntax : '';
    codeEl.textContent = 'Loading...';
    preEl.appendChild(codeEl);
    setConsoleLoading(true);

    fetch('/v2/org/' + ORG_ID + '/forge/instance/' + currentConsoleInstanceId + '/diagnostics/' + item.id, {
        headers: apiHeaders()
    }).then(function(r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        return r.text();
    }).then(function(text) {
        codeEl.textContent = text || '(empty)';
        hljs.highlightElement(codeEl);
        setConsoleLoading(false);
    }).catch(function(err) {
        codeEl.textContent = 'Error: ' + err.message;
        setConsoleLoading(false);
    });
}

function streamDiagToLog(url, preEl, signal) {
    var lineBuffer = '';

    function processChunk(chunk) {
        lineBuffer += chunk;
        var lines = lineBuffer.split('\n');
        lineBuffer = lines.pop(); // keep incomplete last line
        for (var i = 0; i < lines.length; i++) {
            if (lines[i]) appendLogLine(preEl, lines[i]);
        }
        preEl.scrollTop = preEl.scrollHeight;
    }

    return fetch(url, { headers: apiHeaders(), signal: signal }).then(function(r) {
        if (!r.ok) throw new Error('HTTP ' + r.status);
        setConsoleLoading(false);
        var reader = r.body.getReader();
        var decoder = new TextDecoder();
        function read() {
            return reader.read().then(function(result) {
                if (result.done) {
                    if (lineBuffer) appendLogLine(preEl, lineBuffer);
                    return;
                }
                processChunk(decoder.decode(result.value, { stream: true }));
                return read();
            });
        }
        return read();
    });
}

function loadDiagLog(item) {
    var preEl = document.getElementById('log-output');
    preEl.innerHTML = '';
    setConsoleLoading(true);
    var controller = new AbortController();
    followController = controller;
    var url = '/v2/org/' + ORG_ID + '/forge/instance/' + currentConsoleInstanceId + '/diagnostics/' + item.id;

    streamDiagToLog(url, preEl, controller.signal).catch(function(err) {
        setConsoleLoading(false);
        if (err.name !== 'AbortError') {
            preEl.innerHTML = '<div style="color:#f55">Error: ' + escapeHtml(err.message) + '</div>';
        }
    });
}

function startDiagFollow(item) {
    var preEl = document.getElementById('log-output');
    preEl.innerHTML = '';
    setConsoleLoading(true);
    var controller = new AbortController();
    followController = controller;
    var url = '/v2/org/' + ORG_ID + '/forge/instance/' + currentConsoleInstanceId + '/diagnostics/' + item.id + '?follow=true';

    streamDiagToLog(url, preEl, controller.signal).catch(function(err) {
        setConsoleLoading(false);
        if (err.name !== 'AbortError') {
            preEl.innerHTML = '<div style="color:#f55">Error: ' + escapeHtml(err.message) + '</div>';
        }
    });
}

// ── Init ─────────────────────────────────────────────────────────

document.addEventListener('DOMContentLoaded', function() {
    fetchInstanceTypes().then(function() {
        loadInstances();
        startRefresh();
    });
    loadFilesTree();
    currentView = viewFromLocation();
    switchView(currentView, false);
    if (window.location.pathname.replace(/\/+$/, '') !== viewPath(currentView)) {
        window.history.replaceState({ view: currentView }, '', viewPath(currentView));
    }

    document.querySelectorAll('.view-tab').forEach(function(el) {
        el.addEventListener('click', function() {
            switchView(el.getAttribute('data-view'), true);
        });
    });

    document.getElementById('auto-refresh').addEventListener('change', function(e) {
        if (e.target.checked) { startRefresh(); } else { stopRefresh(); }
    });

    document.getElementById('files-refresh').addEventListener('click', function() {
        loadFilesTree();
    });

    window.addEventListener('popstate', function() {
        switchView(viewFromLocation(), false);
    });

    document.getElementById('console-follow').addEventListener('change', function(e) {
        if (e.target.checked) {
            startFollow();
        } else {
            stopFollow();
        }
    });

    // Refit terminal on window resize
    window.addEventListener('resize', function() {
        if (consoleFitAddon && consoleTerm) consoleFitAddon.fit();
    });

    // Ctrl+F opens search when console modal is visible
    document.addEventListener('keydown', function(e) {
        var modalOpen = !document.getElementById('console-modal').hidden;
        if (modalOpen && (e.ctrlKey || e.metaKey) && e.key === 'f') {
            e.preventDefault();
            openConsoleSearch();
        }
        if (e.key === 'Escape' && modalOpen) {
            var searchOpen = !document.getElementById('console-search').hidden;
            if (searchOpen) { closeConsoleSearch(); } else { closeConsoleLog(); }
        }
    });

    // Search input: Enter = next, Shift+Enter = prev
    document.getElementById('console-search-input').addEventListener('keydown', function(e) {
        if (e.key === 'Enter') {
            e.preventDefault();
            if (e.shiftKey) { consoleSearchPrev(); } else { consoleSearchNext(); }
        }
    });

    // Live search as you type
    document.getElementById('console-search-input').addEventListener('input', function() {
        consoleSearchNext();
    });

    document.getElementById('console-source').addEventListener('change', function() {
        if (activeTab === 'console') refreshConsoleLog();
    });

    // Close modal on backdrop click
    document.getElementById('console-modal').addEventListener('click', function(e) {
        if (e.target === this) closeConsoleLog();
    });
});
