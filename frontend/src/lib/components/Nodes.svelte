<script lang="ts">
  import Icon from './Icon.svelte';
  import type { Pilot } from '../state/pilot.svelte';
  let { pilot }: { pilot: Pilot } = $props();
</script>

<div class="section-intro"><div><span class="eyebrow muted">ИНФРАСТРУКТУРА ПИЛОТА</span><h2>Один хост. Один канал.</h2><p>Распределение и будущая генерация находятся на компьютере truekant.</p></div><span class="badge neutral">Конфигурация команды</span></div>
<section class="panel node-detail">
  <div class="panel-head"><div class="node-title"><div class="square-icon large"><Icon name="server" size={30} /></div><div><h3>Ultra · truekant</h3><p>Основной хост Cordova</p></div></div><span class="badge" class:green={pilot.status === 'online'} class:neutral={pilot.status !== 'online'}>{pilot.status === 'online' ? 'Шлюз на связи' : 'Шлюз не подтверждён'}</span></div>
  <div class="spec-grid"><div><span>Процессор</span><strong>Intel Ultra 5 125</strong></div><div><span>Оперативная память</span><strong>64 ГБ DDR5</strong></div><div><span>Графика / ускоритель</span><strong>Intel graphics + NPU</strong></div><div><span>Источник данных</span><strong>Владелец хоста</strong></div></div>
  <div class="node-state-grid"><article><Icon name="globe" size={22} /><h4>Шлюз</h4><p>{pilot.status === 'online' ? 'Health API успешно отвечает.' : 'Ожидаем успешную проверку API.'}</p></article><article><Icon name="chip" size={22} /><h4>Модель</h4><p>Пока не подключена. Работа Intel GPU/NPU ещё не измерена.</p></article><article><Icon name="layers" size={22} /><h4>Канал 01</h4><p>Лимит: один запрос за раз. Живая занятость пока не передаётся API.</p></article></div>
</section>
<div class="two-columns">
  <section class="panel prose"><div class="panel-head"><h3>Как проходит запрос</h3><Icon name="arrow" /></div><ol class="flow-list"><li><span>01</span><div><strong>Клиент на i7</strong><p>Отправляет запрос и получает ответ. Модель локально не запускается.</p></div></li><li><span>02</span><div><strong>Go-шлюз на Ultra</strong><p>Проверяет ключ и допускает один запрос к исполнителю.</p></div></li><li><span>03</span><div><strong>Локальная нода и runner</strong><p>Следующий этап: генерация на том же хосте с отменой и освобождением слота.</p></div></li></ol></section>
  <section class="panel prose"><div class="panel-head"><h3>Что нужно измерить</h3><Icon name="activity" /></div><p>Количество каналов в пилоте зафиксировано. Выбор модели опирается на реальные измерения хоста.</p><ul class="checklist"><li>Память при загрузке и генерации</li><li>Время до первого ответа и скорость</li><li>Поддержку выбранного ускорителя</li><li>Отмену запроса и повторное использование слота</li></ul><div class="subtle-note"><Icon name="info" size={18} /><span>64 ГБ RAM — характеристика хоста, а не результат бенчмарка.</span></div></section>
</div>
