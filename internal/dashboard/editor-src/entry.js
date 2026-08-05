import { EditorState, Compartment } from "@codemirror/state";
import {
  EditorView,
  keymap,
  lineNumbers,
  highlightActiveLine,
  highlightActiveLineGutter,
  drawSelection,
} from "@codemirror/view";
import {
  defaultKeymap,
  history,
  historyKeymap,
  indentWithTab,
} from "@codemirror/commands";
import { yaml } from "@codemirror/lang-yaml";
import {
  syntaxHighlighting,
  defaultHighlightStyle,
  indentOnInput,
  bracketMatching,
  HighlightStyle,
} from "@codemirror/language";
import { tags } from "@lezer/highlight";

// Colors reference the dashboard's own CSS custom properties (style.css),
// so the editor follows the same light/dark switching without a second
// theme definition.
const cssVarTheme = EditorView.theme({
  "&": {
    color: "var(--text)",
    backgroundColor: "var(--panel)",
    height: "100%",
    fontSize: "13px",
  },
  ".cm-content": {
    fontFamily:
      "ui-monospace, 'SF Mono', Menlo, monospace",
    caretColor: "var(--text)",
  },
  ".cm-cursor, .cm-dropCursor": { borderLeftColor: "var(--text)" },
  "&.cm-focused .cm-selectionBackground, .cm-selectionBackground, .cm-content ::selection": {
    backgroundColor: "rgba(91, 157, 255, 0.25)",
  },
  ".cm-activeLine": { backgroundColor: "rgba(91, 157, 255, 0.06)" },
  ".cm-activeLineGutter": { backgroundColor: "rgba(91, 157, 255, 0.1)" },
  ".cm-gutters": {
    backgroundColor: "var(--panel)",
    color: "var(--muted)",
    border: "none",
    borderRight: "1px solid var(--border)",
  },
  ".cm-scroller": { overflow: "auto" },
  "&.cm-editor": {
    border: "1px solid var(--border)",
    borderRadius: "8px",
  },
  "&.cm-editor.cm-focused": { outline: "none" },
});

const yamlHighlight = HighlightStyle.define([
  { tag: tags.comment, color: "var(--muted)", fontStyle: "italic" },
  { tag: tags.propertyName, color: "var(--accent)" },
  { tag: tags.atom, color: "var(--ok)" },
  { tag: tags.bool, color: "var(--ok)" },
  { tag: tags.number, color: "var(--warn)" },
  { tag: tags.string, color: "var(--ok)" },
  { tag: tags.punctuation, color: "var(--muted)" },
  { tag: tags.meta, color: "var(--muted)" },
]);

function create(parent, doc, opts) {
  opts = opts || {};
  const readOnlyCompartment = new Compartment();

  const extensions = [
    lineNumbers(),
    highlightActiveLine(),
    highlightActiveLineGutter(),
    drawSelection(),
    history(),
    indentOnInput(),
    bracketMatching(),
    syntaxHighlighting(yamlHighlight, { fallback: true }),
    syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
    yaml(),
    cssVarTheme,
    readOnlyCompartment.of(EditorState.readOnly.of(!!opts.readOnly)),
    keymap.of([
      ...defaultKeymap,
      ...historyKeymap,
      indentWithTab,
      {
        key: "Mod-s",
        run: () => {
          if (opts.onSave) opts.onSave();
          return true;
        },
      },
    ]),
    EditorView.updateListener.of((update) => {
      if (update.docChanged && opts.onChange) {
        opts.onChange(update.state.doc.toString());
      }
    }),
  ];

  const state = EditorState.create({ doc: doc || "", extensions });
  const view = new EditorView({ state, parent });

  return {
    getValue() {
      return view.state.doc.toString();
    },
    setValue(text) {
      view.dispatch({
        changes: { from: 0, to: view.state.doc.length, insert: text || "" },
      });
    },
    setReadOnly(ro) {
      view.dispatch({
        effects: readOnlyCompartment.reconfigure(
          EditorState.readOnly.of(!!ro)
        ),
      });
    },
    gotoLine(n) {
      const total = view.state.doc.lines;
      const clamped = Math.max(1, Math.min(n, total));
      const line = view.state.doc.line(clamped);
      view.dispatch({
        selection: { anchor: line.from, head: line.to },
        effects: EditorView.scrollIntoView(line.from, { y: "center" }),
      });
      view.focus();
    },
    focus() {
      view.focus();
    },
    destroy() {
      view.destroy();
    },
  };
}

window.HekaEditor = { create };
