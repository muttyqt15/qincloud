// app.js — controld dashboard: the small amount of client state htmx can't do
// declaratively. Native <dialog> open/close for the deploy modal, plus two
// listeners that keep the Deploy button honest.

function qcDeployOpen() {
  const m = document.getElementById("deploy-modal");
  if (m) m.showModal();
}

function qcDeployClose() {
  const m = document.getElementById("deploy-modal");
  if (m) m.close();
}

// Enable "Deploy" only once the image has resolved to a port: the resolve
// fragment emits a [name=port] field when (and only when) the image is real
// and its port is known. Re-evaluated after every image check.
document.body.addEventListener("htmx:afterSwap", function (e) {
  if (!e.target || e.target.id !== "image-result") return;
  const form = e.target.closest("form");
  if (!form) return;
  const hasPort = !!form.querySelector('[name="port"]');
  const btn = form.querySelector('button[type="submit"]');
  if (btn) btn.disabled = !hasPort;
});

// Editing the image invalidates the resolved port IMMEDIATELY — before the
// (async, seconds-long) re-resolve returns. Without this, resolving image A
// then editing to image B and clicking Deploy in one gesture submits B with
// A's port: the port passes range validation, B starts, Caddy dials the wrong
// port, and every request 502s while the deploy reads "live". Clearing the
// port field and disabling submit on every keystroke means a port only ever
// exists for the image currently shown as resolved.
document.body.addEventListener("input", function (e) {
  if (!e.target || e.target.name !== "image") return;
  const form = e.target.closest("form");
  if (!form) return;
  const result = form.querySelector("#image-result");
  if (result) result.innerHTML = "";
  const btn = form.querySelector('button[type="submit"]');
  if (btn) btn.disabled = true;
});

// A successful deploy fires HX-Trigger: deploy-started from the server. Close
// and fully reset the dialog so the next open starts clean (form.reset() does
// not remove the htmx-injected port field, so clear #image-result by hand).
document.body.addEventListener("deploy-started", function () {
  const m = document.getElementById("deploy-modal");
  if (!m) return;
  const form = m.querySelector("form");
  m.close();
  if (!form) return;
  form.reset();
  const result = form.querySelector("#image-result");
  if (result) result.innerHTML = "";
  const btn = form.querySelector('button[type="submit"]');
  if (btn) btn.disabled = true;
});
