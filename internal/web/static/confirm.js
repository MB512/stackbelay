// Asks before submitting forms with a data-confirm attribute (stop and
// restart of a stack). Without JavaScript the form is submitted directly.
// Plain script, no build step.
(function () {
	"use strict";

	document.addEventListener("submit", function (e) {
		var form = e.target;
		var question = form.getAttribute && form.getAttribute("data-confirm");
		if (question && !window.confirm(question)) {
			e.preventDefault();
		}
	});
})();
