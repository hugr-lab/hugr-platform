// A page that fails to draw shows why, and the rest of the console stays (React unmounts the whole root otherwise).
import { Component, type ReactNode } from 'react'
import { Problem } from './ui'

export class Boundary extends Component<{ children: ReactNode; at: string }, { error?: unknown; at: string }> {
  state: { error?: unknown; at: string } = { at: this.props.at }

  static getDerivedStateFromError(error: unknown) {
    return { error }
  }

  // another page is drawn anew
  static getDerivedStateFromProps(props: { at: string }, state: { error?: unknown; at: string }) {
    return props.at !== state.at ? { error: undefined, at: props.at } : null
  }

  render() {
    return this.state.error !== undefined ? <Problem error={this.state.error} /> : this.props.children
  }
}
