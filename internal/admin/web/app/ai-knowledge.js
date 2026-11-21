import { h, api, apiList, post, btn, icon, empty, callout, dialog, confirmDialog, errText, toast, loading, section } from './core.js';

const endpoint = '/api/ai/local-knowledge';
const topics = { fishing: '垂钓', shop: '装备与商店', craft: '制作配方' };
const collections = { item_aliases: '社区别名', notes: '本地心得' };

export function localKnowledgeSection() {
  let snapshot;
  let collection = 'item_aliases';
  let saving = false;
  const tabs = h('div', { class: 'seg', 'aria-label': '资料类型' });
  const results = h('div', { class: 'card ai-knowledge-results', 'aria-live': 'polite' });
  const search = h('input', { class: 'input', type: 'search', 'aria-label': '搜索本地资料', oninput: renderRows });
  const add = btn('添加社区别名', () => edit(), 'primary', { icon: 'plus' });
  const download = btn('备份', exportFile, 'ghost sm', { icon: 'doc', title: '下载 JSON 备份' });
  const file = h('input', { type: 'file', accept: '.json,application/json', hidden: true, onchange: importFile });
  const importer = btn('导入', () => file.click(), 'ghost sm', { title: '从 JSON 文件导入资料' });

  async function load() {
    add.disabled = download.disabled = importer.disabled = true;
    results.replaceChildren(loading(3));
    try {
      snapshot = await api(endpoint);
      add.disabled = download.disabled = importer.disabled = false;
      render();
    } catch (e) {
      results.replaceChildren(empty('本地资料读取失败', errText(e), btn('重试', load, 'sm')));
    }
  }

  async function save(base, document) {
    if (saving) throw new Error('另一项资料正在保存，请稍等');
    saving = true;
    try {
      snapshot = await post(endpoint, { base_revision: base.revision, document });
      render();
    } catch (e) {
      if (e.status === 409) {
        void load();
        throw new Error('其他窗口更新了资料。列表正在刷新，请复制当前草稿后重新打开条目。');
      }
      throw e;
    } finally {
      saving = false;
    }
  }

  function render() {
    const buttons = Object.entries(collections).map(([key, text]) => btn(text, () => {
      collection = key;
      search.value = '';
      render();
      tabs.querySelector('[aria-pressed="true"]').focus();
    }, key === collection ? 'on' : '', { 'aria-pressed': String(key === collection) }));
    tabs.replaceChildren(...buttons);
    add.lastChild.textContent = '添加' + collections[collection];
    search.placeholder = collection === 'item_aliases' ? '搜索简称或物品 ID' : '搜索标题、关键词或来源';
    renderRows();
  }

  function renderRows() {
    if (!snapshot) return;
    const query = search.value.trim().toLowerCase();
    const entries = snapshot.document[collection];
    const matches = entries.flatMap((entry, index) => {
      const searchable = collection === 'item_aliases' ? [entry.alias, entry.item_id]
        : [entry.title, topics[entry.topic], ...entry.keywords, entry.source, entry.text];
      if (!searchable.join(' ').toLowerCase().includes(query)) return [];
      const primary = collection === 'item_aliases' ? entry.alias : entry.title;
      const details = collection === 'item_aliases'
        ? h('code', { class: 'sub mono', text: entry.item_id })
        : h('div', { class: 'sub', text: [topics[entry.topic], entry.source].filter(Boolean).join(' · ') });
      return [h('div', { class: 'list-item ai-knowledge-row' },
        h('div', { class: 'grow ai-knowledge-entry' },
          h('div', { class: 'title', text: primary }), details,
          collection === 'notes' ? h('div', { class: 'ai-knowledge-preview small', text: entry.text }) : null),
        h('div', { class: 'row ai-knowledge-item-actions' },
          btn('编辑', () => edit(index), 'ghost sm', { 'aria-label': '编辑 ' + primary }),
          btn('删除', () => remove(index, primary), 'ghost sm', { 'aria-label': '删除 ' + primary })))];
    });
    results.replaceChildren(...(matches.length ? matches : [
      query ? empty('没有匹配的资料', '试试别的关键词。')
        : empty('还没有' + collections[collection], collection === 'item_aliases'
          ? '把钓友的简称和游戏里的物品对应起来。' : '把常用经验和来源记下来，角色可按授权查询。')]));
  }

  function edit(index) {
    if (!snapshot) return;
    const base = snapshot;
    const kind = collection;
    const original = base.document[kind][index] || {};
    const field = (label, input, help) => h('label', { class: 'field' },
      h('span', { class: 'small text-2', text: label }), input, help ? h('span', { class: 'small muted', text: help }) : null);
    const input = (value, placeholder) => h('input', { class: 'input', required: true, value: value || '', placeholder });
    const feedback = h('div', { role: 'alert', class: 'small ai-knowledge-error', hidden: true });
    let read;
    let fields;
    if (kind === 'item_aliases') {
      const alias = input(original.alias, '例如钓友常用的简称');
      const item = input(original.item_id, '搜索游戏物品，或直接输入 ID');
      const suggestions = itemSuggestions(item);
      fields = [
        field('社区别名', alias, '同一个简称可对应多个型号，助手会请玩家确认。'),
        field('物品名称或 ID', item),
        suggestions,
      ];
      read = () => ({ alias: alias.value.trim(), item_id: item.value.trim() });
    } else {
      const topic = h('select', { class: 'select' }, Object.entries(topics).map(([value, text]) => h('option', { value, text })));
      topic.value = original.topic || 'fishing';
      const title = input(original.title, '这条心得讲什么');
      const keywords = input(original.keywords?.join('，'), '例如 底钓，蚯蚓');
      const source = input(original.source, '例如 个人实测或社区帖子标题');
      const note = h('textarea', { class: 'textarea ai-knowledge-text', rows: 5, required: true, placeholder: '写下具体做法和适用条件' });
      note.value = original.text || '';
      fields = [
        h('div', { class: 'form-grid ai-knowledge-meta' }, field('标题', title), field('资料主题', topic)),
        field('心得正文', note),
        field('关键词', keywords, '用逗号隔开。角色在玩家提到这些词时可以找到这条心得。'),
        field('来源', source),
      ];
      read = () => ({ topic: topic.value, title: title.value.trim(),
        keywords: keywords.value.split(/[,，、]/).map((v) => v.trim()).filter(Boolean),
        text: note.value.trim(), source: source.value.trim() });
    }
    const form = h('form', { class: 'stack', onsubmit: (e) => e.preventDefault() }, fields, feedback);
    dialog({ title: (index === undefined ? '添加' : '编辑') + collections[kind], body: form,
      actions: [{ label: '取消' }, { label: '保存', kind: 'primary', onClick: async () => {
        if (!form.reportValidity()) return true;
        const draft = [...base.document[kind]];
        draft.splice(index ?? draft.length, index === undefined ? 0 : 1, read());
        try {
          await save(base, { ...base.document, [kind]: draft });
          toast('资料已保存');
        } catch (e) {
          feedback.textContent = errText(e);
          feedback.hidden = false;
          return true;
        }
      } }],
    });
  }

  async function remove(index, title) {
    const base = snapshot;
    const kind = collection;
    if (!await confirmDialog({ title: '删除「' + title + '」？', desc: '删除后，角色就查不到这条资料。', confirm: '删除', danger: true })) return;
    try {
      await save(base, { ...base.document, [kind]: base.document[kind].filter((_, i) => i !== index) });
      toast('资料已删除');
    } catch (e) { toast(errText(e), 'bad'); }
  }

  function exportFile() {
    if (!snapshot) return;
    const url = URL.createObjectURL(new Blob([JSON.stringify(snapshot.document, null, 2)], { type: 'application/json' }));
    const link = h('a', { href: url, download: 'rf4-ai-knowledge.json' });
    link.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  async function importFile() {
    const selected = file.files[0];
    const base = snapshot;
    file.value = '';
    if (!selected || !base) return;
    try {
      if (selected.size > 512 * 1024) throw new Error('文件过大，请选择 512 KB 以内的 JSON 备份');
      const doc = JSON.parse(await selected.text());
      if (!Array.isArray(doc?.item_aliases) || !Array.isArray(doc?.notes)) throw new Error('文件缺少别名或心得列表，请选择本页下载的 JSON 备份');
      if (!await confirmDialog({ title: '导入并替换本地资料？',
        desc: '来自文件「' + selected.name + '」的 ' + doc.item_aliases.length + ' 条别名和 ' + doc.notes.length + ' 条心得将替换当前资料。可先下载备份。',
        confirm: '替换资料', danger: true })) return;
      await save(base, doc);
      toast('资料已导入');
    } catch (e) { toast(e instanceof SyntaxError ? '文件不是有效的 JSON' : errText(e), 'bad'); }
  }

  void load();
  return section({
    title: '本地资料',
    actions: [download, importer, file],
    body: h('div', { class: 'stack' },
      h('div', { class: 'row ai-knowledge-toolbar' }, tabs, h('span', { class: 'spacer' }),
        h('div', { class: 'search ai-knowledge-search' }, icon('search'), search), add),
      results,
      h('p', { class: 'small muted', text: '资料随本机存档保存。匹配到的条目可能随对话请求发送给已配置的模型。' })),
  });
}

function itemSuggestions(input) {
  const box = h('div', { class: 'card ai-knowledge-suggestions', hidden: true });
  let timer;
  let request;
  input.addEventListener('input', () => {
    clearTimeout(timer);
    request?.abort();
    box.hidden = true;
    const q = input.value.trim();
    if (q.length < 2) return;
    timer = setTimeout(async () => {
      if (!box.isConnected) return;
      const search = new AbortController();
      request = search;
      try {
        const items = await apiList('/api/catalog/items?limit=6&q=' + encodeURIComponent(q), { signal: search.signal });
        if (search.signal.aborted || !box.isConnected) return;
        box.replaceChildren(...items.map((item) => h('button', { type: 'button', class: 'list-item ai-knowledge-option',
          onclick: () => { input.value = item.item_id; box.hidden = true; input.focus(); } },
        h('span', { class: 'grow', text: item.name || item.item_id }), h('code', { class: 'sub mono', text: item.item_id }))));
        box.hidden = items.length === 0;
      } catch (e) {
        if (!search.signal.aborted) {
          box.replaceChildren(callout('warn', '目录暂时无法读取，可以直接填写物品 ID。'));
          box.hidden = false;
        }
      }
    }, 250);
  });
  return box;
}
