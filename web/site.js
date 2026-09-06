const invite = document.getElementById('invite-text')
if (invite) {
  invite.value = `${invite.dataset.story}\n\nInstall Baton and say hello:\n${location.origin}${location.pathname}`
}

for (const button of document.querySelectorAll('[data-copy]')) {
  button.addEventListener('click', async () => {
    const field = document.getElementById(button.dataset.copy)
    const status = document.getElementById(button.dataset.status)
    const text = field.value ?? field.textContent
    try {
      await navigator.clipboard.writeText(text)
      status.textContent = button.dataset.success
    } catch {
      if (typeof field.select === 'function') {
        field.focus()
        field.select()
      } else {
        const range = document.createRange()
        range.selectNodeContents(field)
        const selection = window.getSelection()
        selection.removeAllRanges()
        selection.addRange(range)
      }
      status.textContent = 'Automatic copy isn’t available. Copy the selected text instead.'
    }
  })
}
