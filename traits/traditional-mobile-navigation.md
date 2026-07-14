# Traditional Mobile Navigation

#ui #navigation #accessibility

This application trait adds a familiar hamburger-driven side navigation pattern for apps that need the primary content to stay focused until navigation is requested.

## Behavior

The top bar shows a compact brand area and a hamburger button. Activating the hamburger opens a side menu above the page content and shows a dark overlay across the rest of the interface.

Users can close the menu by:

- selecting the close icon in the menu
- clicking or tapping the dark overlay
- pressing Escape

When the menu is open, the underlying page should not scroll. The hamburger button should update `aria-expanded` so assistive technology can detect the menu state.

## Layout Contract

Keep the logo inside a fixed navbar footprint. The image may have a large source resolution, but the rendered logo should use explicit width, height, and `object-fit: contain` so it does not stretch the top bar or appear cropped.

The side menu should be predictable:

- fixed to the viewport edge
- full viewport height
- narrow enough to leave the overlay visible
- above the overlay in stacking order
- animated with a short transform transition

## Styling Notes

The overlay should be dark enough to clearly block interaction with the page behind it. Navigation links belong inside the drawer, not scattered across the top bar, so the header remains compact on mobile and desktop.
