<script>
  import { onMount } from 'svelte';
  import { formatBytes, formatEta, sourceName, stateOrder } from './format.js';

  let text = $state('');
  let files = $state([]);
  let target = $state('auto');
  let category = $state('');
  let clients = $state([]);
  let sending = $state(false);
  let results = $state(null);
  let dropError = $state('');
  let dragging = $state(false);

  // Review JDownloader links before downloading (remembered per browser).
  let review = $state(
    (() => {
      try {
        return localStorage.getItem('drop.review') !== '0';
      } catch {
        return true;
      }
    })()
  );
  $effect(() => {
    try {
      localStorage.setItem('drop.review', review ? '1' : '0');
    } catch {}
  });
  let reviewPkgs = $state([]);
  let crawling = $state(false);
  let reviewError = $state('');
  // Per link id: the owner's choice, overriding the suggestion.
  let choice = $state({});
  let busyPkg = $state('');

  let downloads = $state([]);
  let statusErrors = $state({});
  let filter = $state('active');

  let fileInput;
  let textArea;

  const targetInfo = $derived(clients.find((c) => c.target === target));
  const categories = $derived(targetInfo?.categories ?? []);
  const canSend = $derived(!sending && (text.trim() !== '' || files.length > 0));

  const visible = $derived(
    downloads
      .filter((d) => (filter === 'active' ? d.state !== 'done' : filter === 'done' ? d.state === 'done' : true))
      .toSorted((a, b) => stateOrder(a.state) - stateOrder(b.state))
  );
  const counts = $derived({
    active: downloads.filter((d) => d.state !== 'done').length,
    done: downloads.filter((d) => d.state === 'done').length,
  });

  $effect(() => {
    // A category belongs to one client.
    target;
    category = '';
  });

  async function loadConfig() {
    try {
      const r = await fetch('/api/config');
      clients = (await r.json()).clients;
    } catch {
      clients = [];
    }
  }

  async function loadReview() {
    try {
      const r = await fetch('/api/review');
      const out = await r.json();
      reviewPkgs = out.packages;
      crawling = !!out.crawling;
      reviewError = out.error ?? '';
    } catch {
      reviewError = 'not reachable';
    }
  }

  function selected(link) {
    return choice[link.id] ?? link.selected;
  }

  function setAll(pkg, on) {
    for (const l of pkg.links) if (l.availability !== 'OFFLINE') choice[l.id] = on;
  }

  async function reviewPost(path, body) {
    const r = await fetch(path, {
      method: 'POST',
      headers: { 'X-Drop': '1', 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!r.ok) throw new Error((await r.json()).error ?? `HTTP ${r.status}`);
  }

  async function startPkg(pkg) {
    busyPkg = pkg.id;
    try {
      const start = pkg.links.filter(selected).map((l) => l.id);
      const remove = pkg.links.filter((l) => !selected(l)).map((l) => l.id);
      await reviewPost('/api/review/start', { start, remove });
      await Promise.all([loadReview(), loadStatus()]);
    } catch (e) {
      reviewError = String(e.message ?? e);
    } finally {
      busyPkg = '';
    }
  }

  async function discardPkg(pkg) {
    busyPkg = pkg.id;
    try {
      await reviewPost('/api/review/discard', { packages: [pkg.id] });
      await loadReview();
    } catch (e) {
      reviewError = String(e.message ?? e);
    } finally {
      busyPkg = '';
    }
  }

  async function loadStatus() {
    try {
      const r = await fetch('/api/status');
      const s = await r.json();
      downloads = s.downloads;
      statusErrors = s.errors;
    } catch (e) {
      statusErrors = { drop: 'not reachable' };
    }
  }

  function addFiles(list) {
    for (const f of list) {
      if (!files.some((x) => x.name === f.name && x.size === f.size)) files.push(f);
    }
  }

  async function send() {
    if (!canSend) return;
    sending = true;
    dropError = '';
    results = null;
    const form = new FormData();
    form.set('text', text);
    form.set('target', target);
    form.set('category', category);
    form.set('review', review ? '1' : '0');
    for (const f of files) form.append('files', f);
    try {
      const r = await fetch('/api/drop', { method: 'POST', body: form, headers: { 'X-Drop': '1' } });
      const out = await r.json();
      if (!r.ok) {
        dropError = out.error ?? `HTTP ${r.status}`;
      } else {
        results = out.results;
        // Keep what failed in the box, so it can be retried or sent elsewhere.
        const failed = results.filter((x) => !x.ok);
        text = failed.filter((x) => !files.some((f) => f.name === x.item)).map((x) => x.item).join('\n');
        files = files.filter((f) => failed.some((x) => x.item === f.name));
        loadStatus();
        loadReview();
      }
    } catch (e) {
      dropError = String(e);
    } finally {
      sending = false;
    }
  }

  function onKey(e) {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      send();
    }
  }

  function onPaste(e) {
    if (e.clipboardData?.files?.length) {
      e.preventDefault();
      addFiles(e.clipboardData.files);
    }
  }

  onMount(() => {
    // PWA share target (/share?title=&text=&url=).
    const p = new URLSearchParams(location.search);
    if (location.pathname === '/share') {
      text = [p.get('url'), p.get('text')].filter(Boolean).join('\n');
      history.replaceState(null, '', '/');
    }

    loadConfig();
    loadStatus();
    loadReview();
    let timer = setInterval(() => {
      if (document.visibilityState !== 'visible') return;
      loadStatus();
      loadReview();
    }, 2000);

    let depth = 0;
    const enter = (e) => {
      if (e.dataTransfer?.types?.includes('Files')) {
        depth++;
        dragging = true;
      }
    };
    const leave = () => {
      depth = Math.max(0, depth - 1);
      if (depth === 0) dragging = false;
    };
    const over = (e) => e.preventDefault();
    const drop = (e) => {
      e.preventDefault();
      depth = 0;
      dragging = false;
      if (e.dataTransfer?.files?.length) addFiles(e.dataTransfer.files);
      const t = e.dataTransfer?.getData('text/plain');
      if (t && !e.dataTransfer.files.length) text = (text ? text + '\n' : '') + t;
    };
    window.addEventListener('dragenter', enter);
    window.addEventListener('dragleave', leave);
    window.addEventListener('dragover', over);
    window.addEventListener('drop', drop);
    return () => {
      clearInterval(timer);
      window.removeEventListener('dragenter', enter);
      window.removeEventListener('dragleave', leave);
      window.removeEventListener('dragover', over);
      window.removeEventListener('drop', drop);
    };
  });
</script>

{#if dragging}
  <div class="dropzone"><div>Drop .torrent, .nzb or .dlc files</div></div>
{/if}

<main>
  <header>
    <h1>drop</h1>
    <span class="hint">links · magnets · .torrent · .nzb · .dlc</span>
  </header>

  <section class="card">
    <textarea
      bind:this={textArea}
      bind:value={text}
      onkeydown={onKey}
      onpaste={onPaste}
      rows="5"
      placeholder="Paste links or magnets (any text: links are picked out), or drop files anywhere"
    ></textarea>

    {#if files.length}
      <ul class="files">
        {#each files as f, i (f.name + f.size)}
          <li>
            <span>{f.name}</span>
            <span class="muted">{formatBytes(f.size)}</span>
            <button class="icon" title="Remove" onclick={() => files.splice(i, 1)}>×</button>
          </li>
        {/each}
      </ul>
    {/if}

    <div class="controls">
      <div class="seg" role="radiogroup" aria-label="Send to">
        <button class:on={target === 'auto'} onclick={() => (target = 'auto')}>Auto</button>
        {#each clients as c (c.target)}
          <button class:on={target === c.target} onclick={() => (target = c.target)} title={c.error ?? ''}>
            {sourceName(c.target)}{#if c.error}<span class="warn">!</span>{/if}
          </button>
        {/each}
      </div>

      {#if target !== 'auto' && categories.length}
        <select bind:value={category} aria-label="Category">
          <option value="">No category</option>
          {#each categories as c (c)}<option value={c}>{c}</option>{/each}
        </select>
      {/if}

      {#if clients.some((c) => c.target === 'jdownloader')}
        <label class="toggle" title="JDownloader links wait below until you pick the files">
          <input type="checkbox" bind:checked={review} /> Review JDownloader links
        </label>
      {/if}
      <div class="spacer"></div>
      <input bind:this={fileInput} type="file" multiple accept=".torrent,.nzb,.dlc" hidden
        onchange={(e) => { addFiles(e.target.files); e.target.value = ''; }} />
      <button class="ghost" onclick={() => fileInput.click()}>Files…</button>
      <button class="primary" disabled={!canSend} onclick={send}>{sending ? 'Sending…' : 'Send'}</button>
    </div>
    {#if target === 'auto'}
      <p class="muted small">
        Auto: magnets and .torrent → Deluge, .nzb → SABnzbd, all other links → JDownloader.
        {#if !review}JDownloader starts only the relevant files (videos, archives, …; images only for pure albums).{/if}
      </p>
    {/if}

    {#if dropError}<p class="error">{dropError}</p>{/if}
    {#if results}
      <ul class="results">
        {#each results as r, i (i)}
          <li class:fail={!r.ok}>
            <span class="mark">{r.ok ? '✓' : '✕'}</span>
            <span class="item">{r.item}</span>
            {#if r.target}<span class="badge {r.target}">{sourceName(r.target)}</span>{/if}
            {#if r.error}<span class="error small">{r.error}</span>{/if}
          </li>
        {/each}
      </ul>
    {/if}
  </section>

  {#if reviewPkgs.length || crawling || reviewError}
    <section class="card">
      <div class="status-head">
        <h2>Review <span class="muted small">JDownloader link grabber</span></h2>
        {#if crawling}<span class="muted small">JDownloader is still collecting links…</span>{/if}
      </div>
      {#if reviewError}<p class="error small">JDownloader: {reviewError}</p>{/if}
      {#each reviewPkgs as pkg (pkg.id)}
        <div class="pkg">
          <div class="pkg-head">
            <strong class="name" title={pkg.name}>{pkg.name}</strong>
            <span class="muted small">
              {pkg.links.filter(selected).length}/{pkg.links.length} ·
              {formatBytes(pkg.links.filter(selected).reduce((n, l) => n + (l.size || 0), 0))}
            </span>
          </div>
          {#if pkg.auto}
            <p class="muted small">Starts automatically once JDownloader has checked the links.</p>
          {:else}
            <ul class="links">
              {#each pkg.links as l (l.id)}
                <li class:offline={l.availability === 'OFFLINE'}>
                  <label>
                    <input type="checkbox" checked={selected(l)} disabled={l.availability === 'OFFLINE'}
                      onchange={(e) => (choice[l.id] = e.target.checked)} />
                    <span class="name" title={l.url || l.name}>{l.name}</span>
                  </label>
                  <span class="muted small">{l.size ? formatBytes(l.size) : ''}</span>
                  {#if l.availability !== 'ONLINE'}<span class="muted small">{l.availability.toLowerCase().replace('_', ' ')}</span>{/if}
                </li>
              {/each}
            </ul>
            <div class="controls">
              <button class="ghost small" onclick={() => setAll(pkg, true)}>All</button>
              <button class="ghost small" onclick={() => setAll(pkg, false)}>None</button>
              <div class="spacer"></div>
              <button class="ghost" disabled={busyPkg === pkg.id} onclick={() => discardPkg(pkg)}>Discard</button>
              <button class="primary" disabled={busyPkg === pkg.id || !pkg.links.some(selected)} onclick={() => startPkg(pkg)}>
                Download {pkg.links.filter(selected).length}
              </button>
            </div>
          {/if}
        </div>
      {/each}
    </section>
  {/if}

  <section class="card">
    <div class="status-head">
      <h2>Downloads</h2>
      <div class="seg small">
        <button class:on={filter === 'active'} onclick={() => (filter = 'active')}>Active <span class="muted">{counts.active}</span></button>
        <button class:on={filter === 'done'} onclick={() => (filter = 'done')}>Done <span class="muted">{counts.done}</span></button>
        <button class:on={filter === 'all'} onclick={() => (filter = 'all')}>All</button>
      </div>
    </div>

    {#each Object.entries(statusErrors) as [src, msg] (src)}
      <p class="error small">{sourceName(src)}: {msg}</p>
    {/each}

    {#if visible.length === 0}
      <p class="muted">{filter === 'done' ? 'Nothing finished recently.' : 'Nothing downloading.'}</p>
    {:else}
      <ul class="downloads">
        {#each visible as d (d.source + d.id)}
          <li>
            <div class="row1">
              <span class="badge {d.source}">{sourceName(d.source)}</span>
              <span class="name" title={d.name}>{d.name}</span>
              <span class="state {d.state}" title={d.detail ?? ''}>{d.state}</span>
            </div>
            <div class="bar"><div class="fill {d.state}" style="width: {Math.round(d.progress * 1000) / 10}%"></div></div>
            <div class="row2 muted small">
              <span>{Math.floor(d.progress * 100)}%</span>
              {#if d.size}<span>{formatBytes(d.done)} / {formatBytes(d.size)}</span>{/if}
              {#if d.speed > 0}<span>{formatBytes(d.speed)}/s</span>{/if}
              {#if d.eta > 0 && d.state === 'downloading'}<span>{formatEta(d.eta)} left</span>{/if}
              {#if d.category}<span>{d.category}</span>{/if}
              {#if d.detail && d.state === 'error'}<span class="error">{d.detail}</span>{/if}
            </div>
          </li>
        {/each}
      </ul>
    {/if}
  </section>
</main>
