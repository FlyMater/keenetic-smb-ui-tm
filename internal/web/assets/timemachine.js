'use strict';

const tmEndpoint = '/api/time-machine';
const diskEndpoint = '/api/local-disks';
const diskDrafts = new Map();
let diskState;
let diskLoading = false;
let activeDisk = '';
let tmBrowsePath = '';

function tmOption(value, label) {
  const option = document.createElement('option');
  option.value = value;
  option.textContent = label;
  return option;
}

function diskVolume(uuid) {
  return (diskState.volumes || []).find((volume) => volume.uuid.toLowerCase() === uuid);
}

function savedDisk(uuid) {
  return (diskState.disks || []).find((disk) => disk.uuid.toLowerCase() === uuid);
}

function diskEdit(uuid) {
  const saved = savedDisk(uuid);
  return diskDrafts.get(uuid) || {
    uuid,
    publish: !!saved?.publish,
    time_machine: saved?.time_machine ? { ...saved.time_machine } : undefined,
  };
}

function newBackupProfile(uuid) {
  const suffix = uuid.replace(/[^a-z0-9]/g, '');
  const base = 'TimeMachine' + (suffix ? '-' + suffix.slice(0, 32) : '');
  const occupied = new Set([
    diskState.share,
    ...(diskState.disks || []).map((disk) => disk.time_machine?.share),
    ...Array.from(diskDrafts.values(), (disk) => disk.time_machine?.share),
  ].filter(Boolean).map((name) => name.toLowerCase()));
  let name = base;
  for (let i = 2; occupied.has(name.toLowerCase()); i++) name = base + '-' + i;
  return { uuid, enabled: true, folder: '', share: name, user: '', readonly: false, limit_gib: 0, bonjour: true };
}

function diskCell(row, content) {
  const cell = row.insertCell();
  if (content instanceof Node) cell.append(content);
  else cell.textContent = content;
}

function diskCheckbox(label, checked, disabled, onChange) {
  const box = document.createElement('input');
  box.type = 'checkbox';
  box.checked = checked;
  box.disabled = disabled;
  box.setAttribute('aria-label', label);
  box.onchange = () => onChange(box.checked);
  return box;
}

function renderLocalDisks() {
  const rows = $('local-disk-rows');
  rows.replaceChildren();
  const inventory = (diskState.volumes || []).map((volume) => [volume.uuid.toLowerCase(), volume]);
  const connected = new Set(inventory.map(([uuid]) => uuid));
  for (const saved of diskState.disks || []) {
    const uuid = saved.uuid.toLowerCase();
    if (!connected.has(uuid)) inventory.push([uuid, null]);
  }
  if (inventory.length === 0) {
    const cell = rows.insertRow().insertCell();
    cell.colSpan = 5;
    cell.textContent = t('USB-диски не найдены');
  }
  for (const [uuid, volume] of inventory) {
    const disk = diskEdit(uuid);
    const saved = savedDisk(uuid);
    const report = diskState.diagnostics?.[uuid];
    const available = uuid && volume && (!volume.reason || volume.readonly && volume.reason === 'USB volume is mounted read-only');
    const backup = disk.time_machine;
    const label = volume?.mount_point || uuid;
    const row = rows.insertRow();
    diskCell(row, label);
    diskCell(row, volume?.total ? fmtBytes(volume.free) + ' / ' + fmtBytes(volume.total) : '—');
    diskCell(row, diskCheckbox(t('Публиковать по SMB') + ' — ' + label, disk.publish,
      !available && !disk.publish, (publish) => {
        const edit = diskEdit(uuid);
        edit.publish = publish;
        if (!publish && edit.time_machine?.enabled) {
          edit.time_machine.enabled = false;
          if (activeDisk === uuid) activeDisk = '';
        }
        diskDrafts.set(uuid, edit);
        renderLocalDisks();
        showBackupEditor();
      }));
    const backupControls = document.createElement('div');
    backupControls.className = 'row';
    backupControls.append(diskCheckbox('Time Machine — ' + label, !!backup?.enabled,
      !backup?.enabled && (!available || volume.readonly || !disk.publish),
      (enabled) => {
        const edit = diskEdit(uuid);
        edit.time_machine ||= newBackupProfile(uuid);
        edit.time_machine.enabled = enabled;
        if (!enabled) delete edit.time_machine.migration;
        diskDrafts.set(uuid, edit);
        if (enabled || activeDisk === uuid) activeDisk = enabled ? uuid : '';
        renderLocalDisks();
        showBackupEditor();
      }));
    if (backup?.enabled) {
      const edit = document.createElement('button');
      edit.className = 'btn ghost';
      edit.type = 'button';
      edit.textContent = t('Настроить');
      edit.onclick = () => { activeDisk = uuid; showBackupEditor(); };
      backupControls.append(edit);
    }
    diskCell(row, backupControls);
    const status = [];
    if (report?.published && saved?.directory) status.push(diskState.share + '/' + saved.directory);
    if (report?.disk === 'ready') status.push(t('Time Machine доступен'));
    if (report?.disk_error) status.push(t(report.disk_error));
    else if (volume?.reason) status.push(t(volume.reason));
    else if (!volume) status.push(t('Том недоступен'));
    if (diskDrafts.has(uuid)) status.push(t('Есть изменения'));
    diskCell(row, status.join(' · ') || t('Доступен'));
  }
  $('disk-apply').disabled = diskDrafts.size === 0;
  $('tm-import').classList.toggle('hidden', !diskState.legacy || Array.from(diskDrafts.values()).some((disk) => disk.time_machine?.migration));
}

async function loadLocalDisks(force = false) {
  if (diskLoading || !force && diskDrafts.size) return;
  diskLoading = true;
  $('disk-refresh').disabled = true;
  try {
    const incoming = await api(diskEndpoint);
    if (!incoming.legacy && Array.from(diskDrafts.values()).some((disk) => disk.time_machine?.migration)) {
      incoming.legacy = diskState.legacy;
    }
    diskState = incoming;
    $('disk-message').textContent = t(diskState.inventory_error || '');
    renderLocalDisks();
    showBackupEditor();
    return true;
  } catch (error) {
    $('disk-message').textContent = t(error.message);
    return false;
  } finally {
    diskLoading = false;
    $('disk-refresh').disabled = false;
  }
}

function renderBackupUsers(profile) {
  const users = (diskState.users || []).filter((user) => profile.readonly || !user.readonly);
  const options = users.map((user) => tmOption(user.name, user.name));
  if (profile.user && !users.some((user) => user.name === profile.user)) {
    const legacy = savedDisk(activeDisk)?.time_machine;
    if (legacy && !legacy.smb_user && legacy.user === profile.user) {
      options.push(tmOption(legacy.user, legacy.user));
    }
  }
  $('tm-user').replaceChildren(tmOption('', t('Выберите пользователя SMB')), ...options);
  $('tm-user').value = options.some((option) => option.value === profile.user) ? profile.user : '';
  $('tm-no-users').classList.toggle('hidden', options.length !== 0);
}

function showBackupEditor() {
  const backup = activeDisk && diskEdit(activeDisk).time_machine;
  $('tm-form').classList.toggle('hidden', !backup?.enabled);
  $('tm-directory-picker').classList.add('hidden');
  $('tm-details').classList.add('hidden');
  $('tm-message').textContent = '';
  if (!backup?.enabled) return;
  const volume = diskVolume(activeDisk);
  const migration = backup.migration;
  $('tm-title').textContent = 'Time Machine — ' + (volume?.mount_point || activeDisk);
  $('tm-legacy-target').textContent = migration ? diskState.legacy.target : '';
  $('tm-folder').value = backup.folder || '';
  $('tm-share').value = backup.share;
  $('tm-limit').value = backup.limit_gib || 0;
  $('tm-write').checked = !backup.readonly;
  $('tm-bonjour').checked = !!backup.bonjour;
  $('tm-create').checked = !!backup.create_folder;
  for (const id of ['tm-share', 'tm-folder', 'tm-create', 'tm-browse', 'tm-write', 'tm-user']) {
    $(id).disabled = !!migration;
  }
  $('tm-migration').classList.toggle('hidden', !migration);
  if (migration) {
    $('tm-owner').checked = migration.change_owner;
    $('tm-bundle').replaceChildren(
      tmOption('', t('Выберите резервную копию')),
      ...diskState.legacy.bundles.map((bundle) => tmOption(bundle, bundle)),
    );
    $('tm-bundle').value = migration.bundle;
  }
  renderBackupUsers(backup);
  showOwnershipPath();
}

function readBackupEditor() {
  const edit = diskEdit(activeDisk);
  const backup = edit.time_machine;
  const migration = backup.migration;
  edit.time_machine = {
    ...backup,
    folder: $('tm-folder').value,
    share: $('tm-share').value.trim(),
    user: migration ? backup.user : $('tm-user').value,
    readonly: !$('tm-write').checked,
    limit_gib: Number($('tm-limit').value || 0),
    bonjour: $('tm-bonjour').checked,
    create_folder: $('tm-create').checked,
  };
  if (migration) {
    edit.time_machine.migration = { bundle: $('tm-bundle').value, change_owner: $('tm-owner').checked };
  }
  diskDrafts.set(activeDisk, edit);
  return edit.time_machine;
}

function showOwnershipPath() {
  const backup = activeDisk && diskEdit(activeDisk).time_machine;
  const migration = backup?.migration;
  $('tm-bundle').required = !!(migration?.change_owner && diskState.legacy.bundles.length);
  $('tm-owner-path').textContent = migration?.change_owner
    ? [diskState.legacy.target, migration.bundle].filter(Boolean).join(' / ')
    : '';
}

function validateBackupDrafts() {
  const selected = activeDisk;
  for (const [uuid, edit] of diskDrafts) {
    if (!edit.time_machine?.enabled) continue;
    activeDisk = uuid;
    showBackupEditor();
    if (!$('tm-form').reportValidity()) return false;
  }
  activeDisk = selected;
  showBackupEditor();
  return true;
}

async function listBackupFolders(path) {
  const uuid = activeDisk;
  try {
    const folders = await api(tmEndpoint + '/folders?' + new URLSearchParams({ uuid, path }));
    if (activeDisk !== uuid) return;
    tmBrowsePath = path;
    $('tm-current-directory').textContent = path || t('Корень тома');
    $('tm-directories').replaceChildren(...folders.map((name) => tmOption(name, name)));
    $('tm-up').disabled = !path;
    $('tm-enter').disabled = !folders.length;
    $('tm-directory-picker').classList.remove('hidden');
    $('tm-message').textContent = '';
  } catch (error) {
    $('tm-message').textContent = t(error.message);
  }
}

function showBackupDiagnostics(report) {
  const labels = {
    smb: 'Служба SMB', disk: 'USB-том', permissions: 'Права доступа',
    xattr: 'Расширенные атрибуты', limit_gib: 'Лимит копий, ГиБ', smb_url: 'Адрес SMB',
    bonjour: 'Bonjour', disk_error: 'Проверка тома', bonjour_error: 'Проверка Bonjour',
    apply_error: 'Ошибка применения', recovery_error: 'Ошибка восстановления',
  };
  const states = {
    running: 'работает', stopped: 'остановлен', disabled: 'отключено', ready: 'Доступен',
    waiting: 'ожидание', pending: 'ожидает проверки', verified: 'проверено',
    skipped: 'пропущено', published: 'опубликовано',
  };
  $('tm-details').textContent = Object.entries(labels)
    .filter(([key]) => Object.hasOwn(report, key))
    .map(([key, label]) => t(label) + ' — ' + t(states[report[key]] || report[key]))
    .join('\n');
  $('tm-details').classList.remove('hidden');
}

$('disk-refresh').onclick = () => loadLocalDisks(true);
$('disk-apply').onclick = async () => {
  if (!validateBackupDrafts()) return;
  const section = $('smb-disks');
  section.inert = true;
  $('disk-message').textContent = t('Применяю настройки USB-дисков…');
  try {
    await api(diskEndpoint, {
      method: 'POST',
      body: JSON.stringify({ disks: Array.from(diskDrafts.values()) }),
    });
    diskDrafts.clear();
    if (await loadLocalDisks(true) && !diskState.inventory_error) {
      $('disk-message').textContent = t('Сохранено.');
    }
  } catch (error) {
    $('disk-message').textContent = t(error.message);
  } finally {
    section.inert = false;
  }
};

$('tm-import').onclick = () => {
  const legacy = diskState.legacy;
  if (!(diskState.users || []).some((user) => user.name === legacy.profile.user && !user.readonly)) {
    $('disk-message').textContent = t('Для переноса нужен включённый пользователь SMB с правом записи.');
    return;
  }
  activeDisk = legacy.profile.uuid.toLowerCase();
  const edit = diskEdit(activeDisk);
  edit.time_machine = {
    ...legacy.profile,
    migration: { bundle: legacy.bundles.length === 1 ? legacy.bundles[0] : '', change_owner: false },
  };
  diskDrafts.set(activeDisk, edit);
  renderLocalDisks();
  showBackupEditor();
};

$('tm-cancel-import').onclick = () => {
  const edit = diskEdit(activeDisk);
  if (edit.publish === !!savedDisk(activeDisk)?.publish) diskDrafts.delete(activeDisk);
  else {
    delete edit.time_machine;
    diskDrafts.set(activeDisk, edit);
  }
  activeDisk = '';
  renderLocalDisks();
  showBackupEditor();
};

$('tm-form').onsubmit = (event) => event.preventDefault();
$('tm-form').addEventListener('input', (event) => {
  if (event.target.id === 'tm-directories') return;
  const profile = readBackupEditor();
  if (event.target.id === 'tm-write') {
    renderBackupUsers(profile);
    readBackupEditor();
  }
  showOwnershipPath();
  renderLocalDisks();
});

$('tm-browse').onclick = () => listBackupFolders($('tm-folder').value);
$('tm-up').onclick = () => listBackupFolders(tmBrowsePath.split('/').slice(0, -1).join('/'));
$('tm-enter').onclick = () => listBackupFolders([tmBrowsePath, $('tm-directories').value].filter(Boolean).join('/'));
$('tm-select-directory').onclick = () => {
  $('tm-folder').value = tmBrowsePath;
  $('tm-directory-picker').classList.add('hidden');
  readBackupEditor();
  renderLocalDisks();
};

$('tm-check').onclick = () => {
  if (!$('tm-form').reportValidity()) return;
  const uuid = activeDisk;
  const profile = { ...diskEdit(uuid).time_machine, uuid };
  const results = {
    connected: 'Подключение SMB проверено.',
    preflight: 'Том и права проверены. Примените настройки.',
    check_in_finder: 'Проверьте подключение через Finder.',
    ownership_pending: 'Права будут проверены после смены владельца.',
  };
  runButtonAction([$('tm-check'), $('tm-diagnostics')], $('tm-message'), 'Проверяю подключение Time Machine…', async () => {
    const result = await api(tmEndpoint + '/check?uuid=' + encodeURIComponent(uuid), {
      method: 'POST', body: JSON.stringify(profile),
    });
    if (activeDisk === uuid) $('tm-message').textContent = t(results[result.status] || result.status);
  });
};

$('tm-diagnostics').onclick = () => {
  const uuid = activeDisk;
  runButtonAction([$('tm-diagnostics'), $('tm-check')], $('tm-message'), 'Загрузка настроек…', async () => {
    const report = await api(tmEndpoint + '/diagnostics?uuid=' + encodeURIComponent(uuid));
    if (activeDisk === uuid) {
      $('tm-message').textContent = '';
      showBackupDiagnostics(report);
    }
  });
};
