const form = document.querySelector('#booking-form');
const status = document.querySelector('#status');
const button = document.querySelector('#reserve');
const prices = { ceramics: 48, printmaking: 65 };
form.addEventListener('input', () => {
  document.querySelector('#total').textContent = '$' + prices[form.elements.workshop.value] * Number(form.elements.seats.value);
});
form.addEventListener('submit', async event => {
  event.preventDefault();
  button.disabled = true;
  status.dataset.state = 'pending';
  status.textContent = 'Reserving your seats…';
  try {
    const response = await fetch('/api/reservation', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ workshop: form.elements.workshop.value, seats: Number(form.elements.seats.value), name: form.elements.name.value, email: form.elements.email.value })
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || 'Reservation failed');
    status.dataset.state = 'success';
    status.textContent = `Reserved ${result.seats} seat(s) for ${result.workshop}. Total $${result.total}. Reference ${result.reference}.`;
  } catch (error) {
    console.error('Reservation failed:', error.message);
    status.dataset.state = 'error';
    status.textContent = `Could not reserve: ${error.message}. Please try again.`;
  } finally {
    button.disabled = false;
  }
});
