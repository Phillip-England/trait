# Application Control System

#ui #keyboard #productivity #accessibility

This application trait establishes a controller system for web apps that should be fast to operate without forcing users to rely only on pointer navigation.

## Purpose

Use this trait when an app has repeated actions that users perform many times in a session: searching, moving through records, selecting the current item, copying or exporting a result, checking out a selection, or opening reference material.

The exact shortcuts may be defined by the product, but the application should have a clear control map and a dedicated controls page that explains it.

## Interaction Contract

Keyboard controls should be global only when the user is not typing in a form field. If focus is inside an input, textarea, select, or editable region, normal text editing must win.

Common controls should be reachable with familiar single keys:

- focus search
- move to the next item with arrow keys
- move to the previous item with arrow keys
- open the current item with Enter
- add or remove the current item from a selection with Space
- copy or export the current item
- copy, export, or check out the current selection
- open the controls reference page
- return from the controls reference page to the primary workflow

Prefer controls that match what non-technical users already expect. Arrow keys should navigate through lists before introducing editor-style shortcuts such as J and K.

The controller should reuse existing UI actions where possible. For example, a keyboard shortcut that copies selected items should trigger the same behavior as the visible copy-selected button.

## Controls Page

Provide a dedicated page that lists the available controls in plain language. The page should be linked from the main workflow, not hidden in documentation outside the app.

Each control row should show:

- the key or gesture
- the action name
- a short result-oriented description

The page should be useful even if the final shortcut map changes later.

The controls page must also document how to leave it. A controls reference should never trap the user away from the primary workflow; returning to the app should be a first-class control.

## Accessibility Notes

Do not override browser or assistive-technology behavior when modifier keys are used. Ignore shortcuts with Control, Command, Alt, or Option unless the app explicitly owns that combination.

When a shortcut changes application state, update the same status or live-region feedback used by the pointer action. Users should be able to confirm that the command worked without guessing.
