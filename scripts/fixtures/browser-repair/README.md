# Fieldwork workshop booking

A disposable local demo. No real customer data, reservations, payments, dependencies or external services.

Run `python3 server.py`; stdout prints the chosen loopback URL. The page serves files without a build step.

The reservation API is `POST /api/reservations`, with JSON `workshop`, integer `seats` (1–4), `name` and `email`. It returns 201 with a reference and a server-calculated total. Ceramics costs $48 per person; printmaking costs $65.

Development shutdown: send `POST /__dev/stop` from the browser on the app's origin, then await the managed process exiting with code 0. This stops only this disposable server. A cancelled command has unknown effects in Meldra; use the application's clean shutdown instead of cancelling or detaching it. Restart after editing.
