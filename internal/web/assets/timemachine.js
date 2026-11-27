'use strict';

const tmEndpoint = '/api/time-machine';
let tmBrowsePath = '';
let tmPending = false;

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

async function loadTimeMachine() {
  if (tmPending) return;
  $('tm-password').value = '';
  $('tm-create').checked = false;
  $('tm-directory-picker').classList.add('hidden');
  try {
    const { profile, diagnostics } = await api(tmEndpoint);
    $('tm-enabled').checked = !!profile?.enabled;
    $('tm-bonjour').checked = profile?.bonjour ?? true;
    $('tm-write').checked = !(profile?.readonly ?? false);
    $('tm-share').value = profile?.share || 'TimeMachine';
    $('tm-user').value = profile?.user || 'timemachine';
    $('tm-folder').value = profile?.folder || '';
    $('tm-limit').value = profile?.limit_gib || 0;
    tmRenderDiagnostics(diagnostics);
    tmRenderVolumes(await api(tmEndpoint + '/volumes'), profile?.uuid);
    tmMessage('');
  } catch (error) {
    tmMessage(error.message);
  }
}

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
  if (tmPending || !$('tm-form').reportValidity()) return;
  tmPending = true;
  $('tm-apply').disabled = $('tm-check').disabled = true;
  const payload = {
    enabled: $('tm-enabled').checked,
    uuid: $('tm-volume').value,
    folder: $('tm-folder').value,
    share: $('tm-share').value.trim(),
    user: $('tm-user').value.trim(),
    password: $('tm-password').value,
    readonly: !$('tm-write').checked,
    limit_gib: Number($('tm-limit').value || 0),
    bonjour: $('tm-bonjour').checked,
    create_folder: $('tm-create').checked,
  };
  try {
    const result = await api(tmEndpoint + '/' + action, { method: 'POST', body: JSON.stringify(payload) });
    tmMessage({
      applied: 'Настройки применены',
      connected: 'Подключение SMB проверено',
      preflight: 'Предварительные проверки пройдены. Для проверки SMB примените настройки и введите пароль.',
      preflight_pending_account_or_folder: 'Предварительные проверки пройдены. Права и xattr будут проверены при применении.',
    }[result.status]);
    tmRenderDiagnostics(result.diagnostics);
    if (action === 'apply') $('tm-create').checked = false;
  } catch (error) {
    tmMessage(error.message);
  } finally {
    $('tm-password').value = '';
    $('tm-apply').disabled = $('tm-check').disabled = false;
    tmPending = false;
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
