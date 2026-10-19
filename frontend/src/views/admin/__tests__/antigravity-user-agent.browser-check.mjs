// Run against the local mock preview and an existing kimi-webbridge session.
// This is an opt-in browser check, not part of the unit-test runner.
import assert from 'node:assert/strict'
import { mkdir, writeFile } from 'node:fs/promises'
import { resolve } from 'node:path'

const session = 'sub2api-antigravity-ua-agent32'
const out = resolve('src/views/admin/__tests__/.ua-evidence')
await mkdir(out, { recursive: true })
async function command(action, args = {}) {
  const response = await fetch('http://127.0.0.1:10086/command', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ action, args, session }),
    signal: AbortSignal.timeout(30000),
  })
  const result = await response.json()
  assert.equal(result.ok, true, JSON.stringify(result))
  return result.data
}
const evaluate = async (code) => {
  const result = await command('evaluate', { code })
  assert.notEqual(result.type, 'error', JSON.stringify(result))
  return result.value
}
const state = () => evaluate(`(async () => { const evidence = await (await fetch('/__ua-test__/evidence')).json(); return JSON.stringify({ ua: document.querySelector('#antigravity-user-agent').value, version: document.querySelector('#antigravity-user-agent-version').value, invalid: document.querySelector('#antigravity-user-agent').getAttribute('aria-invalid'), disabled: document.querySelector('#antigravity-user-agent').disabled, error: document.querySelector('#antigravity-user-agent-error')?.textContent.trim(), saves: evidence.saves.map(p => ({ua:p.antigravity_user_agent,version:p.antigravity_user_agent_version})), body: document.body.innerText }); })()`)
const read = async () => JSON.parse(await state())
async function save(expectedCount) {
  await command('click', { selector: 'form button[type="submit"]' })
  await evaluate(`(async () => { for(let i=0;i<100;i++){const e=await(await fetch('/__ua-test__/evidence')).json(); if(e.saves.length>=${expectedCount}&&!document.querySelector('#antigravity-user-agent').disabled)return true;await new Promise(r=>setTimeout(r,50))}throw new Error('Save did not settle');})()`)
}
async function capture(name) {
  await evaluate(`document.querySelectorAll('button[aria-label="Close notification"]').forEach(button => button.click())`)
  // Let viewport and Vue layout changes settle even when the browser tab is hidden.
  await new Promise(resolve => setTimeout(resolve, 200))
  await evaluate(`document.querySelector('#antigravity-user-agent').scrollIntoView({block:'center',behavior:'instant'})`)
  return command('screenshot', { path: resolve(out, name), format: 'png' })
}
const evidence = []
await command('navigate', { url: 'http://127.0.0.1:43127/admin/settings' })
await evaluate(`(async () => {for(let i=0;i<100;i++){const tabs=[...document.querySelectorAll('[role=tab]')];const gateway=tabs.find(e=>e.textContent.includes('网关服务'));if(gateway&&document.querySelector('#antigravity-user-agent')){document.querySelector('.driver-popover-close-btn')?.click();gateway.click();return true;}await new Promise(r=>setTimeout(r,50));}throw new Error('Settings did not load');})()`)
await command('cdp', { method: 'Emulation.setDeviceMetricsOverride', params: { width: 1440, height: 1000, deviceScaleFactor: 1, mobile: false } })
let current = await read()
assert.equal(current.ua, 'antigravity/hub/2.23.0 darwin/arm64')
assert.equal(current.version, '1.23.2')
evidence.push({ check: 'GET renders complete UA and numeric version', result: 'pass' })
await capture('desktop-initial.png')
await command('fill', { selector: '#antigravity-user-agent', value: '  custom-client/3.0 linux/amd64  ' })
await save(1)
current = await read()
assert.deepEqual(current.saves[0], { ua: 'custom-client/3.0 linux/amd64', version: '1.23.2' })
assert.equal(current.ua, 'custom-client/3.0 linux/amd64')
assert.ok(current.body.includes('设置保存成功'))
evidence.push({ check: 'Edit → PUT trimmed payload → response echo + success', result: 'pass', payload: current.saves[0] })
await command('fill', { selector: '#antigravity-user-agent', value: '' })
await save(2)
current = await read()
assert.deepEqual(current.saves[1], { ua: '', version: '1.23.2' })
assert.equal(current.ua, '')
evidence.push({ check: 'Clear sends explicit empty string, retains numeric version', result: 'pass' })
await command('fill', { selector: '#antigravity-user-agent', value: 'client/中文' })
await command('click', { selector: 'form button[type="submit"]' })
current = await read()
assert.equal(current.invalid, 'true')
assert.ok(current.error.includes('可打印 ASCII'))
assert.equal(current.saves.length, 2)
await capture('desktop-invalid.png')
evidence.push({ check: 'Non-ASCII blocks PUT with inline error and alert', result: 'pass' })
await command('fill', { selector: '#antigravity-user-agent', value: 'client/3.0' })
const prevented = await evaluate(String.raw`(() => {const data=new DataTransfer();data.setData('text','client/3.0\r\nInjected: yes');const event=new ClipboardEvent('paste',{clipboardData:data,bubbles:true,cancelable:true});document.querySelector('#antigravity-user-agent').dispatchEvent(event);return event.defaultPrevented;})()`)
assert.equal(prevented, true)
current = await read()
assert.equal(current.ua, 'client/3.0')
evidence.push({ check: 'CRLF paste rejected before native single-line normalization', result: 'pass' })
await command('fill', { selector: '#antigravity-user-agent', value: 'a'.repeat(513) })
current = await read()
assert.ok(current.error.includes('512'))
await command('fill', { selector: '#antigravity-user-agent', value: 'client/3.0' })
await evaluate(`fetch('/__ua-test__/reject-next-save').then(r=>r.json())`)
await save(3)
current = await read()
assert.equal(current.ua, 'client/3.0')
assert.equal(current.disabled, false)
assert.ok(current.body.includes('Antigravity User-Agent could not be saved. Try again.'))
evidence.push({ check: 'Backend failure surfaced, edited value retained, save unlocked', result: 'pass' })
await save(4)
current = await read()
assert.ok(current.body.includes('设置保存成功'))
evidence.push({ check: 'Retry succeeds', result: 'pass' })
await command('cdp', { method: 'Emulation.setDeviceMetricsOverride', params: { width: 390, height: 844, deviceScaleFactor: 1, mobile: true } })
await command('fill', { selector: '#antigravity-user-agent', value: '' })
await capture('mobile-empty.png')
await evaluate(`document.documentElement.classList.add('dark')`)
await command('fill', { selector: '#antigravity-user-agent', value: 'client/中文' })
await capture('mobile-dark-invalid.png')
const layout = JSON.parse(await evaluate(`(() => {const input=document.querySelector('#antigravity-user-agent'); const r=input.getBoundingClientRect();return JSON.stringify({viewport:innerWidth,left:r.left,right:r.right,width:r.width,invalid:input.getAttribute('aria-invalid'),font:getComputedStyle(input).fontFamily,label:document.querySelector('label[for="antigravity-user-agent"]').textContent.trim()});})()`))
assert.ok(layout.left >= 0 && layout.right <= layout.viewport)
assert.ok(layout.font.includes('mono'))
evidence.push({ check: '390px layout remains within viewport with semantic label and monospace input', result: 'pass', layout })
await command('fill', { selector: '#antigravity-user-agent', value: 'client/3.0' })
await evaluate(`document.documentElement.classList.remove('dark')`)
await command('cdp', { method: 'Emulation.clearDeviceMetricsOverride', params: {} })
await writeFile(resolve(out, 'interaction-results.json'), JSON.stringify(evidence, null, 2))
console.log(JSON.stringify(evidence, null, 2))
