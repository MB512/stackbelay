// Small form helpers for every page. Plain script, no build step; without
// JavaScript all forms work as they are (the server checks everything).
//
// - data-confirm="<question>": ask before submitting (e.g. stop or
//   restart a stack, an update without backup). With
//   data-confirm-when="<field>=<value>" only if that field of the form has
//   the value (e.g. the job type of a schedule).
// - Busy state: a plain form that is submitted (the browser waits for the
//   next page) marks its submit button busy: a spinner (class "busy", see
//   style.css), disabled against double submits, and the text of the
//   form's data-busy attribute if it has one ("Checking access…"). HTMX
//   forms are left alone: HTMX sets the class "htmx-request" on them
//   during the request, which style.css shows the same way.
(function () {
	"use strict";

	function confirmApplies(form) {
		var when = form.getAttribute("data-confirm-when");
		if (!when) {
			return true;
		}
		var i = when.indexOf("=");
		var field = form.elements[when.slice(0, i)];
		return field !== undefined && field.value === when.slice(i + 1);
	}

	function markBusy(form, button) {
		if (!button) {
			return;
		}
		button.classList.add("busy");
		var text = form.getAttribute("data-busy");
		if (text) {
			button.setAttribute("data-label", button.textContent);
			button.textContent = text;
		}
		// Disable after the browser has taken the form data (a disabled
		// button would not be part of it).
		window.setTimeout(function () {
			button.disabled = true;
		}, 0);
	}

	document.addEventListener("submit", function (e) {
		var form = e.target;
		if (!form.getAttribute) {
			return;
		}
		var question = form.getAttribute("data-confirm");
		if (question && confirmApplies(form) && !window.confirm(question)) {
			e.preventDefault();
			return;
		}
		// HTMX prevents the default submit of its forms before this
		// listener runs (it listens on the form itself).
		if (e.defaultPrevented) {
			return;
		}
		markBusy(form, e.submitter || form.querySelector("button[type=submit], button:not([type])"));
	});

	// Back to a page from the browser's cache: undo busy buttons.
	window.addEventListener("pageshow", function (e) {
		if (!e.persisted) {
			return;
		}
		var busy = document.querySelectorAll("button.busy");
		for (var i = 0; i < busy.length; i++) {
			busy[i].classList.remove("busy");
			busy[i].disabled = false;
			if (busy[i].hasAttribute("data-label")) {
				busy[i].textContent = busy[i].getAttribute("data-label");
				busy[i].removeAttribute("data-label");
			}
		}
	});
})();
