// Removing a database.
//
// Removal is offered in three places, because it is the kind of thing people
// look for wherever they happen to be: on the overview row, in the database
// header, and in Settings. All three go through here, so they confirm the
// same way and say the same thing.
//
// The confirmation is a second press on the same button rather than a dialog.
// A dialog on top of a page is heavier than the decision warrants, and it
// trains people to dismiss dialogs. Two presses on a button that has changed
// its own label cannot happen by accident, and the label carries the
// reassurance that matters: the database itself is untouched.

import { h, text } from '/dom.js';
import { api } from '/api.js';
import { toast, refreshClusters, navigate } from '/app.js';

/**
 * Build a remove button for one database.
 *
 * @param {object} cluster    the cluster view being removed
 * @param {object} [options]
 * @param {string} [options.label]  the resting label
 * @param {string} [options.class]  extra classes for the button
 * @param {Function} [options.onDone]  called after a successful removal
 */
export function removeButton(cluster, options = {}) {
  const restingLabel = options.label || 'Remove';
  let armed = false;
  let disarm;

  const button = h('button', {
    class: options.class || 'btn btn-danger btn-sm',
    type: 'button',
    title: `Remove ${cluster.name} from idxa. The database itself is untouched.`,
    onclick: async (event) => {
      // Removing from a row inside a link or a clickable row must not also
      // navigate.
      event.stopPropagation();
      event.preventDefault();

      if (!armed) {
        armed = true;
        button.textContent = 'Press again to remove';
        button.classList.add('btn-arming');
        // Disarm on its own, so a button left mid-confirm cannot be
        // triggered by a stray press minutes later.
        disarm = setTimeout(() => {
          armed = false;
          button.textContent = restingLabel;
          button.classList.remove('btn-arming');
        }, 5000);
        return;
      }

      clearTimeout(disarm);
      button.disabled = true;
      button.textContent = 'Removing…';

      try {
        await api.delete(`/api/clusters/${encodeURIComponent(cluster.id)}`);
        toast(`Removed ${cluster.name}. The database itself is untouched.`, 'ok');
        await refreshClusters();
        if (options.onDone) options.onDone();
      } catch (err) {
        toast(err.message, 'error');
        button.disabled = false;
        armed = false;
        button.textContent = restingLabel;
        button.classList.remove('btn-arming');
      }
    },
  }, text(restingLabel));

  return button;
}

/** Remove a database and return to the overview afterwards. */
export function removeAndGoHome(cluster, options = {}) {
  return removeButton(cluster, { ...options, onDone: () => navigate('/') });
}
