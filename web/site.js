const invite = document.getElementById('invite-text')
if (invite) {
  invite.textContent = `${invite.dataset.story}\n\nPut a wave in your Omarchy bar:\n${location.origin}${location.pathname}`
}

for (const button of document.querySelectorAll('[data-copy]')) {
  button.addEventListener('click', async () => {
    const field = document.getElementById(button.dataset.copy)
    const status = document.getElementById(button.dataset.status)
    const text = field.textContent
    try {
      await navigator.clipboard.writeText(text)
      status.textContent = button.dataset.success
      button.classList.add('done')
      setTimeout(() => button.classList.remove('done'), 1600)
    } catch {
      const range = document.createRange()
      range.selectNodeContents(field)
      const selection = window.getSelection()
      selection.removeAllRanges()
      selection.addRange(range)
      status.textContent = 'Automatic copy isn’t available. Copy the selected text instead.'
    }
  })
}
