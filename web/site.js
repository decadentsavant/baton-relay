const invite = document.getElementById('invite')
const status = document.getElementById('copy-status')
invite?.addEventListener('click', async () => {
  const text = `${invite.dataset.story} Put a wave in your Omarchy bar. ${location.origin}${location.pathname}`
  try {
    await navigator.clipboard.writeText(text)
    status.textContent = 'Invite copied. Share it when you feel like it.'
  } catch {
    status.textContent = 'Copy this invite to share:'
    let field = document.getElementById('manual-invite')
    if (!field) {
      field = document.createElement('textarea')
      field.id = 'manual-invite'
      field.readOnly = true
      field.setAttribute('aria-label', 'Invite to copy')
      status.after(field)
    }
    field.value = text
    field.focus()
    field.select()
  }
})
if (location.origin !== 'https://relay.baton.buzz') {
  document.getElementById('custom-relay').hidden = false
  document.getElementById('relay-origin').textContent = location.origin
}
