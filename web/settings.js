(() => {
  'use strict';

  const EVENTS = [
    ['critical', 'Critique'],
    ['warning', 'Attention'],
    ['offline', 'Hors ligne'],
    ['recovery', 'Retour à la normale / en ligne'],
    ['reminder', 'Rappels (critique qui dure)'],
  ];

  const root = document.getElementById('settings');
  const dash = document.getElementById('dash');
  const nav = document.getElementById('nav-settings');

  let model = null; // réglages en cours d'édition
  let saved = '';   // JSON de la dernière version enregistrée
  let newToken = '';
  let clearToken = false;
  let chats = null;
  let busy = false;

  // ---------- utilitaires ----------
  function h(tag, props, ...kids) {
    const e = document.createElement(tag);
    for (const [k, v] of Object.entries(props || {})) {
      if (v === false || v == null) continue;
      if (k === 'class') e.className = v;
      else if (k === 'text') e.textContent = v;
      else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
      else if (v === true) e.setAttribute(k, '');
      else e.setAttribute(k, v);
    }
    for (const c of kids.flat()) if (c != null && c !== false) e.append(c);
    return e;
  }
  let uid = 0;
  const nextId = (p) => p + '-' + (++uid);

  async function api(method, path, body) {
    const opt = { method, headers: { 'X-Lezard': '1' }, cache: 'no-store', credentials: 'same-origin' };
    if (body !== undefined) {
      opt.headers['Content-Type'] = 'application/json';
      opt.body = JSON.stringify(body);
    }
    const r = await fetch(path, opt);
    if (r.status === 204) return null;
    let data = null;
    try { data = await r.json(); } catch (e) { /* corps vide */ }
    if (!r.ok) {
      const err = new Error((data && data.error) || ('Erreur HTTP ' + r.status));
      err.status = r.status;
      throw err;
    }
    return data;
  }

  function field(label, input, hint) {
    const id = input.id || (input.id = nextId('f'));
    return h('div', { class: 'field' },
      h('label', { for: id, text: label }),
      input,
      hint ? h('p', { class: 'hint', text: hint }) : null);
  }

  function checkbox(label, checked, onchange, cls) {
    const id = nextId('c');
    const input = h('input', { type: 'checkbox', id, checked: !!checked });
    input.addEventListener('change', () => onchange(input.checked));
    return h('label', { class: cls || 'check', for: id }, input, h('span', { text: label }));
  }

  function numInput(value, opts, onchange) {
    const i = h('input', { type: 'number', inputmode: 'decimal', step: opts.step || '1', min: opts.min, max: opts.max, placeholder: opts.placeholder || '' });
    i.value = value == null ? '' : String(value);
    i.addEventListener('input', () => onchange(i.value === '' ? null : Number(i.value)));
    return i;
  }

  function textInput(value, opts, onchange) {
    const i = h('input', { type: opts.type || 'text', maxlength: opts.max || 64, placeholder: opts.placeholder || '', autocomplete: 'off', spellcheck: 'false' });
    i.value = value || '';
    i.addEventListener('input', () => onchange(i.value));
    return i;
  }

  const hostLabel = (host) => {
    const hs = model && model.hosts.find((x) => x.host === host);
    return hs && hs.name ? hs.name : host.charAt(0).toUpperCase() + host.slice(1);
  };

  // ---------- état « modifié » ----------
  function snapshot() {
    return JSON.stringify({ m: model, t: newToken, c: clearToken });
  }
  function dirty() { return model && snapshot() !== saved; }
  function updateBar() {
    const bar = root.querySelector('.save-bar');
    if (!bar) return;
    const d = dirty();
    bar.querySelector('.save-state').textContent = d ? 'Modifications non enregistrées' : 'Tout est enregistré';
    bar.classList.toggle('is-dirty', d);
    bar.querySelector('button.primary').disabled = !d || busy;
  }
  function changed() { updateBar(); }

  function flash(msg, kind) {
    const el = root.querySelector('.save-msg');
    if (!el) return;
    el.textContent = msg;
    el.className = 'save-msg ' + (kind || '');
  }

  // ---------- écrans ----------
  async function show() {
    root.replaceChildren(h('p', { class: 'muted', text: 'Chargement…' }));
    try {
      const sess = await api('GET', '/api/session');
      if (!sess.admin) return renderDisabled();
      if (!sess.logged_in) return renderLogin();
      await loadSettings();
    } catch (e) {
      root.replaceChildren(h('div', { class: 'panel' }, h('p', { text: 'Impossible de joindre Lézard : ' + e.message })));
    }
  }

  function header(withLogout) {
    return h('div', { class: 'settings-head' },
      h('div', {},
        h('h2', { text: 'Réglages' }),
        h('p', { class: 'muted small', text: 'Alertes Telegram, seuils et noms des machines' })),
      h('div', { class: 'settings-tools' },
        h('a', { class: 'btn txt', href: '#' , text: '← Tableau de bord' }),
        withLogout ? h('button', { class: 'btn txt', type: 'button', text: 'Se déconnecter', onclick: logout }) : null));
  }

  function renderDisabled() {
    root.replaceChildren(header(false), h('div', { class: 'panel' },
      h('h3', { text: 'Réglages désactivés' }),
      h('p', { text: 'Pour gérer le bot et les alertes depuis cette page, définis un mot de passe administrateur (10 caractères minimum) dans le fichier .env : LEZARD_ADMIN_PASSWORD, puis redémarre le conteneur.' })));
  }

  function renderLogin(msg) {
    const pass = h('input', { type: 'password', autocomplete: 'current-password', required: true, id: nextId('p') });
    const err = h('p', { class: 'err', role: 'alert', text: msg || '' });
    const form = h('form', { class: 'panel login' },
      h('h3', { text: 'Connexion' }),
      field('Mot de passe administrateur', pass),
      err,
      h('button', { class: 'btn primary', type: 'submit', text: 'Se connecter' }));
    form.addEventListener('submit', async (ev) => {
      ev.preventDefault();
      err.textContent = '';
      try {
        await api('POST', '/api/login', { password: pass.value });
        pass.value = '';
        await loadSettings();
      } catch (e) {
        err.textContent = e.message;
        pass.select();
      }
    });
    root.replaceChildren(header(false), form);
    pass.focus();
  }

  async function logout() {
    if (dirty() && !confirm('Des modifications ne sont pas enregistrées. Se déconnecter quand même ?')) return;
    try { await api('POST', '/api/logout'); } catch (e) { /* sans importance */ }
    model = null;
    renderLogin();
  }

  async function loadSettings() {
    try {
      const data = await api('GET', '/api/settings');
      setModel(data);
      render();
    } catch (e) {
      if (e.status === 401) return renderLogin();
      throw e;
    }
  }

  function setModel(data) {
    model = data;
    newToken = '';
    clearToken = false;
    saved = snapshot();
  }

  function render() {
    root.replaceChildren(
      header(true),
      botCard(),
      recipientsCard(),
      rulesCard(),
      hostsCard(),
      h('div', { class: 'save-bar' },
        h('span', { class: 'save-state' }),
        h('span', { class: 'save-msg', role: 'status' }),
        h('button', { class: 'btn txt', type: 'button', text: 'Annuler', onclick: () => { if (!dirty() || confirm('Annuler les modifications ?')) loadSettings(); } }),
        h('button', { class: 'btn primary', type: 'button', text: 'Enregistrer', onclick: save })));
    updateBar();
  }

  // ---------- bot ----------
  function botCard() {
    const tg = model.telegram;
    let status;
    if (clearToken) status = h('p', { class: 'note warn', text: 'Le jeton sera oublié à l’enregistrement.' });
    else if (tg.has_token) status = h('p', { class: 'note ok', text: 'Bot enregistré : ' + (tg.bot || '(nom inconnu)') + ', jeton ' + tg.token_hint });
    else status = h('p', { class: 'note', text: 'Aucun bot enregistré.' });

    const tok = h('input', { type: 'password', autocomplete: 'off', spellcheck: 'false', placeholder: tg.has_token ? 'Laisser vide pour garder le jeton actuel' : '123456789:AAE…', id: nextId('t') });
    tok.value = newToken;
    tok.addEventListener('input', () => { newToken = tok.value.trim(); if (newToken) clearToken = false; changed(); });

    return h('section', { class: 'panel' },
      h('h3', { text: 'Bot Telegram' }),
      status,
      field(tg.has_token ? 'Remplacer le jeton du bot' : 'Jeton du bot', tok,
        'Donné par @BotFather (commande /newbot). Il est vérifié auprès de Telegram à l’enregistrement et n’est plus jamais réaffiché.'),
      h('div', { class: 'row' },
        checkbox('Envoyer les alertes', tg.enabled, (v) => { tg.enabled = v; changed(); }, 'check switch'),
        tg.has_token && !clearToken
          ? h('button', { class: 'btn txt danger', type: 'button', text: 'Oublier le jeton', onclick: () => { if (confirm('Oublier le jeton du bot ? Les alertes seront coupées.')) { clearToken = true; tg.enabled = false; newToken = ''; render(); } } })
          : null));
  }

  // ---------- destinataires ----------
  function recipientsCard() {
    const list = h('div', { class: 'recipients' });
    if (!model.recipients.length) {
      list.append(h('p', { class: 'muted', text: 'Aucun destinataire : personne ne reçoit les alertes.' }));
    }
    model.recipients.forEach((rc, i) => list.append(recipientBox(rc, i)));

    const tools = h('div', { class: 'row' },
      h('button', { class: 'btn txt', type: 'button', text: '+ Ajouter un destinataire', onclick: () => { addRecipient('', ''); } }),
      h('button', { class: 'btn txt', type: 'button', text: 'Trouver mes chats', onclick: findChats }));

    const found = h('div', { class: 'chats' });
    if (chats) {
      if (!chats.length) {
        found.append(h('p', { class: 'note', text: 'Aucun chat trouvé. Envoie /start à ton bot dans Telegram (ou ajoute-le au groupe et écris-y un message), puis relance la recherche.' }));
      } else {
        found.append(h('p', { class: 'hint', text: 'Conversations qui ont récemment écrit au bot :' }));
        for (const c of chats) {
          const already = model.recipients.some((r) => r.chat_id === c.id);
          found.append(h('div', { class: 'chat-row' },
            h('span', {}, h('b', { text: c.title || c.id }), h('span', { class: 'muted', text: ' · ' + (c.kind || 'chat') + ' · ' + c.id })),
            already ? h('span', { class: 'muted small', text: 'déjà ajouté' })
              : h('button', { class: 'btn txt', type: 'button', text: 'Ajouter', onclick: () => addRecipient(c.title || 'Chat ' + c.id, c.id) })));
        }
      }
    }

    return h('section', { class: 'panel' },
      h('h3', { text: 'Destinataires' }),
      h('p', { class: 'hint', text: 'Chaque destinataire choisit ce qu’il reçoit : types d’alerte, machines, et une plage calme facultative.' }),
      list, tools, found);
  }

  function addRecipient(name, chat) {
    model.recipients.push({
      id: '', name: name || 'Nouveau destinataire', chat_id: chat || '', enabled: true,
      events: ['critical', 'warning', 'offline', 'recovery'], hosts: [],
      quiet_from: '', quiet_to: '', quiet_critical: true,
    });
    render();
    const boxes = root.querySelectorAll('.recipient');
    const last = boxes[boxes.length - 1];
    if (last) { last.scrollIntoView({ block: 'center' }); const i = last.querySelector('input[type=text]'); if (i) i.focus(); }
  }

  async function findChats() {
    if (!model.telegram.has_token) {
      flash('Enregistre d’abord le jeton du bot.', 'err');
      return;
    }
    flash('Recherche des chats…');
    try {
      const d = await api('POST', '/api/telegram/chats', {});
      chats = d.chats || [];
      flash('');
      render();
    } catch (e) {
      flash(e.message, 'err');
    }
  }

  function recipientBox(rc, i) {
    const hostsAll = rc.hosts.length === 0;
    const hostChips = h('div', { class: 'chips', hidden: hostsAll });
    for (const hs of model.hosts) {
      hostChips.append(checkbox(hostLabel(hs.host), rc.hosts.includes(hs.host), (v) => {
        rc.hosts = v ? [...rc.hosts, hs.host].sort() : rc.hosts.filter((x) => x !== hs.host);
        changed();
      }, 'chip'));
    }
    const radioName = nextId('hosts');
    const radio = (label, val) => {
      const id = nextId('r');
      const inp = h('input', { type: 'radio', name: radioName, id, checked: hostsAll === val });
      inp.addEventListener('change', () => {
        if (!inp.checked) return;
        if (val) { rc.hosts = []; hostChips.hidden = true; }
        else { hostChips.hidden = false; }
        changed();
      });
      return h('label', { class: 'check', for: id }, inp, h('span', { text: label }));
    };

    const evs = h('div', { class: 'chips' });
    for (const [key, label] of EVENTS) {
      evs.append(checkbox(label, rc.events.includes(key), (v) => {
        rc.events = v ? [...rc.events, key] : rc.events.filter((x) => x !== key);
        changed();
      }, 'chip'));
    }

    const qf = h('input', { type: 'time', step: '60' });
    qf.value = rc.quiet_from;
    const qt = h('input', { type: 'time', step: '60' });
    qt.value = rc.quiet_to;
    qf.addEventListener('input', () => { rc.quiet_from = qf.value; changed(); });
    qt.addEventListener('input', () => { rc.quiet_to = qt.value; changed(); });

    const testBtn = h('button', { class: 'btn txt', type: 'button', text: 'Envoyer un test' });
    testBtn.addEventListener('click', async () => {
      if (!model.telegram.has_token) { flash('Enregistre d’abord le jeton du bot.', 'err'); return; }
      if (!rc.chat_id) { flash('Renseigne l’identifiant du chat de ' + rc.name + '.', 'err'); return; }
      testBtn.disabled = true;
      flash('Envoi du test à ' + rc.name + '…');
      try {
        await api('POST', '/api/telegram/test', { chat_id: rc.chat_id });
        flash('Message de test envoyé à ' + rc.name + '.', 'ok');
      } catch (e) {
        flash(rc.name + ' : ' + e.message, 'err');
      } finally {
        testBtn.disabled = false;
      }
    });

    return h('fieldset', { class: 'recipient' + (rc.enabled ? '' : ' is-off') },
      h('legend', {}, rc.name || 'Destinataire'),
      h('div', { class: 'grid2' },
        field('Nom', textInput(rc.name, { max: 40 }, (v) => { rc.name = v; changed(); })),
        field('Identifiant du chat', textInput(rc.chat_id, { placeholder: '123456789 ou -100…', max: 70 }, (v) => { rc.chat_id = v.trim(); changed(); }))),
      h('div', { class: 'sub-block' }, h('p', { class: 'label', text: 'Alertes reçues' }), evs),
      h('div', { class: 'sub-block' },
        h('p', { class: 'label', text: 'Machines' }),
        h('div', { class: 'row' }, radio('Toutes', true), radio('Seulement celles-ci', false)),
        hostChips),
      h('div', { class: 'sub-block' },
        h('p', { class: 'label', text: 'Plage calme (facultative)' }),
        h('div', { class: 'row' },
          h('label', { class: 'inline' }, 'de ', qf), h('label', { class: 'inline' }, 'à ', qt),
          rc.quiet_from || rc.quiet_to
            ? h('button', { class: 'btn txt', type: 'button', text: 'Retirer', onclick: () => { rc.quiet_from = ''; rc.quiet_to = ''; render(); } })
            : null),
        checkbox('Les alertes critiques passent quand même', rc.quiet_critical, (v) => { rc.quiet_critical = v; changed(); })),
      h('div', { class: 'row end' },
        checkbox('Actif', rc.enabled, (v) => { rc.enabled = v; changed(); }, 'check switch'),
        testBtn,
        h('button', { class: 'btn txt danger', type: 'button', text: 'Supprimer', onclick: () => {
          if (confirm('Supprimer ' + (rc.name || 'ce destinataire') + ' ?')) { model.recipients.splice(i, 1); render(); }
        } })));
  }

  // ---------- règles ----------
  function rulesCard() {
    const r = model.rules;
    return h('section', { class: 'panel' },
      h('h3', { text: 'Règles' }),
      h('div', { class: 'grid3' },
        field('Seuil d’attention (°C)', numInput(r.warn, { step: '0.5', min: -50, max: 150 }, (v) => { r.warn = v; changed(); })),
        field('Seuil critique (°C)', numInput(r.crit, { step: '0.5', min: -50, max: 150 }, (v) => { r.crit = v; changed(); })),
        field('Hystérésis (°C)', numInput(r.hyst, { step: '0.5', min: 0, max: 20 }, (v) => { r.hyst = v; changed(); }),
          'Pour redescendre d’un niveau, il faut passer sous le seuil moins cet écart.'),
        field('Délai de confirmation (min)', numInput(r.delay_min, { step: '0.5', min: 0, max: 60 }, (v) => { r.delay_min = v; changed(); }),
          'Attention et retours sont envoyés si l’état tient ce délai. Critique et Hors ligne partent tout de suite.'),
        field('Rappel en critique (min)', numInput(r.repeat_min, { step: '5', min: 0, max: 1440 }, (v) => { r.repeat_min = v; changed(); }),
          '0 = jamais. Sinon de 5 à 1440 minutes.')));
  }

  // ---------- machines ----------
  function colorPicker(hs) {
    const name = nextId('col');
    const box = h('div', { class: 'swatches', role: 'radiogroup', 'aria-label': 'Couleur de ' + hs.host });
    for (let n = 1; n <= 12; n++) {
      const id = nextId('c');
      const inp = h('input', { type: 'radio', name, id, value: String(n), 'aria-label': 'Couleur ' + n, checked: hs.color === n });
      inp.addEventListener('change', () => { hs.color = n; changed(); });
      const dot = h('span', { class: 'sw-dot' });
      dot.style.background = 'var(--s' + n + ')';
      box.append(h('label', { for: id, class: 'swatch' }, inp, dot));
    }
    return box;
  }

  function hostsCard() {
    const r = model.rules;
    if (!model.hosts.length) {
      return h('section', { class: 'panel' }, h('h3', { text: 'Machines' }),
        h('p', { class: 'muted', text: 'Aucune machine n’a encore envoyé de relevé.' }));
    }
    const rows = model.hosts.map((hs) => h('tr', {},
      h('td', {}, h('code', { text: hs.host }), hs.seen ? null : h('span', { class: 'muted small', text: ' (muette)' })),
      h('td', {}, colorPicker(hs)),
      h('td', {}, textInput(hs.name, { max: 40, placeholder: hs.host.charAt(0).toUpperCase() + hs.host.slice(1) }, (v) => { hs.name = v; changed(); })),
      h('td', {}, numInput(hs.warn, { step: '0.5', placeholder: String(r.warn) }, (v) => { hs.warn = v; changed(); })),
      h('td', {}, numInput(hs.crit, { step: '0.5', placeholder: String(r.crit) }, (v) => { hs.crit = v; changed(); }))));
    return h('section', { class: 'panel' },
      h('h3', { text: 'Machines' }),
      h('p', { class: 'hint', text: 'Couleur = celle de la machine dans les cartes et les graphiques. Seuils vides = seuils généraux. Renseigner les deux pour une machine qui chauffe naturellement plus.' }),
      h('div', { class: 'table-wrap hosts-table' },
        h('table', {},
          h('thead', {}, h('tr', {}, h('th', { text: 'Machine' }), h('th', { text: 'Couleur' }), h('th', { text: 'Nom affiché' }), h('th', { text: 'Attention (°C)' }), h('th', { text: 'Critique (°C)' }))),
          h('tbody', {}, rows))));
  }

  // ---------- enregistrement ----------
  async function save() {
    if (busy) return;
    busy = true;
    updateBar();
    flash(newToken ? 'Vérification du jeton auprès de Telegram…' : 'Enregistrement…');
    try {
      const body = {
        telegram: { enabled: model.telegram.enabled, token: newToken, clear_token: clearToken },
        recipients: model.recipients,
        rules: model.rules,
        hosts: model.hosts,
      };
      const data = await api('PUT', '/api/settings', body);
      setModel(data);
      render();
      flash('Réglages enregistrés.', 'ok');
    } catch (e) {
      if (e.status === 401) { renderLogin('Session expirée : reconnecte-toi (les modifications non enregistrées sont perdues).'); return; }
      flash(e.message, 'err');
    } finally {
      busy = false;
      updateBar();
    }
  }

  // ---------- navigation ----------
  function route() {
    const onSettings = location.hash === '#reglages';
    root.hidden = !onSettings;
    dash.hidden = onSettings;
    nav.setAttribute('aria-current', onSettings ? 'page' : 'false');
    if (onSettings) show();
    window.scrollTo(0, 0);
  }

  window.addEventListener('hashchange', (ev) => {
    if (location.hash !== '#reglages' && dirty() && !confirm('Des modifications ne sont pas enregistrées. Quitter les réglages ?')) {
      history.replaceState(null, '', ev.oldURL);
      return;
    }
    route();
  });
  window.addEventListener('beforeunload', (ev) => { if (dirty()) { ev.preventDefault(); ev.returnValue = ''; } });
  route();
})();
