# Viewport Application Shell

#ui #layout #navigation

This application trait designs a web app as a fixed viewport workspace instead of a long document page.

## Purpose

Use this trait when the primary workflow should be immediately visible without requiring the user to scroll the whole page. The interface should feel like an application: controls, list navigation, and the active detail view are all available from the first screen.

## Layout Contract

The page should reserve space for the global header, then give the remaining viewport height to the main application shell.

The shell should contain:

- a compact toolbar for the page title, primary context, and reference links
- a searchable list or navigation pane
- a detail or work pane for the selected item

The document itself should not be the main scrolling surface. Instead, panes that can overflow should scroll internally.

## Interaction Notes

Users should be able to move through items, read details, and perform primary actions without losing the surrounding controls. The selected item list and the detail reader should stay visible as peers whenever the viewport has enough room.

On small screens, preserve the same principle with stacked panes: keep the search/list area and detail area inside the viewport, and let each pane manage its own overflow.

## Caution

Do not apply this trait to marketing pages, articles, documentation, or other content-first experiences where natural page scrolling is expected. This trait is for tool-like workflows where persistent controls matter more than document flow.
