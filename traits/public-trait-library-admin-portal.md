# Public Trait Library With Admin Authoring

#traits #admin #markdown #library #portal

Use this trait when the app publishes reusable traits to everyone while keeping creation and maintenance behind an admin portal.

## Public Contract

The public library should be available without login:

```text
/traits
/traits/{slug}
```

Visitors should be able to:

- view the full list of traits
- search by title, tag, and markdown content
- filter by tag
- open a single trait
- copy one trait
- select multiple traits and copy them together

The public library must not expose create, edit, delete, or configuration controls.

## Admin Contract

Trait management should live behind authentication:

```text
/admin
/admin/new
/admin/edit/{slug}
/admin/delete/{slug}
```

Admins should be able to:

- see every trait
- create a new trait from a title and markdown body
- edit an existing trait's markdown
- delete a trait

All admin routes must require the protected admin portal trait.

## Storage Contract

Each trait should be stored as a plain markdown file:

```text
traits/{slug}.md
```

The first `# Heading` is the trait title. Tags are ordinary markdown text using labels such as `#docker`, `#sqlite`, `#admin`, and `#ui`.

Slugs should be generated from the title, limited to lowercase letters, numbers, and dashes. New traits should avoid overwriting existing files by generating a unique slug.

## Rendering Contract

The public reader should render markdown to HTML for reading, but copying should preserve the original markdown. The markdown source remains the durable format.

## Navigation Contract

The app should have three clear surfaces:

- `/` showcases the app
- `/traits` is the public library
- `/admin` is the protected authoring portal

