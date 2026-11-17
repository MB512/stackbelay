// Asks before submitting forms with a data-confirm attribute (e.g. stop and
// restart of a stack, an update without backup). With
// data-confirm-when="<field>=<value>" it asks only if that field of the
// form has the value (e.g. the job type of a schedule). Without JavaScript
// the form is submitted directly; the server enforces what must be
// confirmed. Plain script, no build step.
(function () {
	"use strict";

	function applies(form) {
		var when = form.getAttribute("data-confirm-when");
		if (!when) {
			return true;
		}
		var i = when.indexOf("=");
		var field = form.elements[when.slice(0, i)];
		return field !== undefined && field.value === when.slice(i + 1);
	}

	document.addEventListener("submit", function (e) {
		var form = e.target;
		var question = form.getAttribute && form.getAttribute("data-confirm");
		if (question && applies(form) && !window.confirm(question)) {
			e.preventDefault();
		}
	});
})();
