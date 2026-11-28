'use strict';

const tmEndpoint = '/api/time-machine';
let tmBrowsePath = '';
let tmUsers = [];
let tmProfile = null;
let tmLegacy = null;
let tmMigrating = false;
const tmActionButtons = [$('tm-apply'), $('tm-check')];

function tmMessage(message) {
  $('tm-message').textContent = t(message);
}

function tmOption(value, text) {
  const option = document.createElement('option');
  option.value = value;
  option.textContent = text;
  return option;
}

function tmRenderVolumes(volumes, selected) {
  const picker = $('tm-volume');
  const rows = $('tm-volumes');
  picker.replaceChildren(tmOption('', t('Выберите том')));
  rows.replaceChildren();
  for (const volume of volumes) {
    const row = document.createElement('tr');
    for (const value of [
      volume.mount_point, volume.filesystem, volume.uuid || '—', fmtBytes(volume.total),
      fmtBytes(volume.free), t(volume.reason || 'Доступен'),
    ]) {
      const cell = document.createElement('td');
      cell.textContent = value;
      row.append(cell);
    }
    rows.append(row);
    if (!volume.reason) picker.append(tmOption(volume.uuid, volume.mount_point));
  }
  if (selected && !Array.from(picker.options).some((option) => option.value === selected)) {
    picker.append(tmOption(selected, t('Том недоступен') + ' (' + selected + ')'));
  }
  picker.value = selected || '';
}

function tmRenderDiagnostics(report) {
  const labels = {
    smb: 'Служба SMB', disk: 'USB-том', permissions: 'Права доступа',
    xattr: 'Расширенные атрибуты', limit_gib: 'Лимит копий, ГиБ',
    smb_url: 'Адрес SMB', bonjour: 'Bonjour', disk_error: 'Проверка тома',
    bonjour_error: 'Проверка Bonjour', apply_error: 'Ошибка применения',
    recovery_error: 'Ошибка восстановления',
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

function tmRenderUsers(selected) {
  const picker = $('tm-user');
  const users = tmUsers.filter((user) => !$('tm-write').checked || !user.readonly);
  const dedicated = tmProfile && !tmProfile.smb_user && tmProfile.user;
  picker.replaceChildren(tmOption('', t('Выберите пользователя SMB')));
  for (const user of users) picker.append(tmOption(user.name, user.name));
  if (dedicated && !tmUsers.some((user) => user.name === dedicated)) {
    picker.append(tmOption(dedicated, dedicated));
  }
  picker.value = Array.from(picker.options).some((option) => option.value === selected) ? selected : '';
  picker.required = $('tm-enabled').checked;
  $('tm-no-users').classList.toggle('hidden', picker.options.length > 1);
}

function tmSetProfile(profile) {
  $('tm-enabled').checked = !!profile?.enabled;
  $('tm-bonjour').checked = profile?.bonjour ?? true;
  $('tm-write').checked = !(profile?.readonly ?? false);
  $('tm-share').value = profile?.share || 'TimeMachine';
  $('tm-folder').value = profile?.folder || '';
  $('tm-limit').value = profile?.limit_gib || 0;
  tmRenderUsers(profile?.user);
}

function tmSetMigrating(active) {
  tmMigrating = active;
  $('tm-import').classList.toggle('hidden', active);
  $('tm-migration').classList.toggle('hidden', !active);
  for (const id of ['tm-volume', 'tm-folder', 'tm-share', 'tm-user', 'tm-enabled', 'tm-write', 'tm-create', 'tm-browse']) {
    $(id).disabled = active;
  }
  $('tm-owner').checked = false;
  $('tm-owner-path').textContent = '';
  $('tm-bundle').required = false;
}

async function loadTimeMachine() {
  if ($('tm-apply').disabled) return;
  $('tm-create').checked = false;
  $('tm-directory-picker').classList.add('hidden');
  try {
    const { profile, diagnostics, users, legacy } = await api(tmEndpoint);
    tmUsers = users || [];
    tmProfile = profile;
    tmLegacy = legacy;
    tmSetMigrating(false);
    tmSetProfile(profile);
    $('tm-legacy').classList.toggle('hidden', !legacy);
    if (legacy) {
      $('tm-legacy-target').textContent =
        'smb://' + legacy.host + '/' + legacy.profile.share + ' · ' + legacy.target + ' · ' + legacy.profile.user;
    }
    tmRenderDiagnostics(diagnostics);
    tmRenderVolumes(await api(tmEndpoint + '/volumes'), profile?.uuid);
    tmMessage('');
  } catch (error) {
    tmMessage(error.message);
  }
}

function tmOwnershipChanged() {
  const bundle = $('tm-bundle').value;
  $('tm-bundle').required = $('tm-owner').checked && tmLegacy.bundles.length > 0;
  $('tm-owner-path').textContent = $('tm-owner').checked
    ? [tmLegacy.target, bundle ? tmLegacy.target + '/' + bundle : ''].filter(Boolean).join(' · ')
    : '';
}

$('tm-import').onclick = () => {
  if (!tmUsers.some((user) => user.name === tmLegacy.profile.user && !user.readonly)) {
    tmMessage('Для переноса нужен включённый пользователь SMB с правом записи.');
    return;
  }
  tmSetMigrating(true);
  tmSetProfile(tmLegacy.profile);
  $('tm-volume').value = tmLegacy.profile.uuid;
  $('tm-directory-picker').classList.add('hidden');
  $('tm-create').checked = false;
  const bundles = tmLegacy.bundles;
  $('tm-bundle').replaceChildren(
    tmOption('', t(bundles.length ? 'Выберите резервную копию' : 'Резервные копии не найдены')),
    ...bundles.map((bundle) => tmOption(bundle, bundle)),
  );
  if (bundles.length === 1) $('tm-bundle').value = bundles[0];
  tmMessage('');
};
$('tm-cancel-import').onclick = loadTimeMachine;
$('tm-owner').onchange = tmOwnershipChanged;
$('tm-bundle').onchange = tmOwnershipChanged;
$('tm-write').onchange = () => tmRenderUsers($('tm-user').value);
$('tm-enabled').onchange = () => { $('tm-user').required = $('tm-enabled').checked; };

async function tmListFolders(path) {
  const uuid = $('tm-volume').value;
  if (!uuid) {
    tmMessage('Выберите том');
    return;
  }
  try {
    const folders = await api(tmEndpoint + '/folders?' + new URLSearchParams({ uuid, path }));
    if ($('tm-volume').value !== uuid) return;
    tmBrowsePath = path;
    $('tm-current-directory').textContent = path || t('Корень тома');
    $('tm-directories').replaceChildren(...folders.map((name) => tmOption(name, name)));
    $('tm-up').disabled = !path;
    $('tm-enter').disabled = !folders.length;
    $('tm-directory-picker').classList.remove('hidden');
    tmMessage('');
  } catch (error) {
    tmMessage(error.message);
  }
}

async function tmAction(action) {
  if ($('tm-apply').disabled || !$('tm-form').reportValidity()) return;
  const payload = {
    enabled: $('tm-enabled').checked,
    uuid: $('tm-volume').value,
    folder: $('tm-folder').value,
    share: $('tm-share').value.trim(),
    user: $('tm-user').value.trim(),
    readonly: !$('tm-write').checked,
    limit_gib: Number($('tm-limit').value || 0),
    bonjour: $('tm-bonjour').checked,
    create_folder: $('tm-create').checked,
    ...(tmMigrating ? { migration: { bundle: $('tm-bundle').value, change_owner: $('tm-owner').checked } } : {}),
  };
  const progress = action === 'apply' ? 'Применяю настройки Time Machine…' : 'Проверяю подключение Time Machine…';
  let migrated = false;
  await runButtonAction(tmActionButtons, $('tm-message'), progress, async () => {
    const result = await api(tmEndpoint + '/' + action, { method: 'POST', body: JSON.stringify(payload) });
    tmMessage({
      applied: 'Настройки применены',
      connected: 'Подключение SMB проверено',
      preflight: 'Предварительные проверки пройдены. Примените настройки и подключитесь в Finder.',
      check_in_finder: 'Права на каталог проверены. Подключитесь в Finder с паролем пользователя SMB.',
      ownership_pending: 'Владелец изменится при применении. Затем мастер проверит запись в каталог.',
    }[result.status]);
    tmRenderDiagnostics(result.diagnostics);
    if (action === 'apply') {
      $('tm-create').checked = false;
      migrated = tmMigrating;
    }
  });
  if (migrated) {
    await loadTimeMachine();
    tmMessage('Существующий ресурс перенесён.');
  }
}

$('tm-form').addEventListener('submit', (event) => {
  event.preventDefault();
  tmAction('apply');
});
$('tm-check').onclick = () => tmAction('check');
$('tm-diagnostics').onclick = async () => {
  try {
    tmRenderDiagnostics(await api(tmEndpoint + '/diagnostics'));
    tmRenderVolumes(await api(tmEndpoint + '/volumes'), $('tm-volume').value);
    tmMessage('');
  } catch (error) {
    tmMessage(error.message);
  }
};
$('tm-volume').onchange = () => {
  $('tm-folder').value = '';
  $('tm-directory-picker').classList.add('hidden');
};
$('tm-browse').onclick = () => tmListFolders('');
$('tm-up').onclick = () => tmListFolders(tmBrowsePath.split('/').slice(0, -1).join('/'));
$('tm-enter').onclick = () => tmListFolders([tmBrowsePath, $('tm-directories').value].filter(Boolean).join('/'));
$('tm-select-directory').onclick = () => {
  $('tm-folder').value = tmBrowsePath;
  $('tm-directory-picker').classList.add('hidden');
};
