# typed: false
# frozen_string_literal: true

# This file was generated by GoReleaser. DO NOT EDIT.
class Graphjin < Formula
  desc "Build APIs in 5 minutes. An automagical GraphQL to SQL compiler."
  homepage "https://graphjin.com"
  version "0.15.91"
  license "Apache-2.0"
  bottle :unneeded

  if OS.mac? && Hardware::CPU.intel?
    url "https://github.com/dosco/graphjin/releases/download/v0.15.91/graphjin_0.15.91_Darwin_x86_64.tar.gz"
    sha256 "59fb7050371ae3fadb8e2064f711e2be6f1a1d9e33d3dd8eb9a1bea712518f9d"
  end
  if OS.mac? && Hardware::CPU.arm?
    url "https://github.com/dosco/graphjin/releases/download/v0.15.91/graphjin_0.15.91_Darwin_arm64.tar.gz"
    sha256 "56d37ba80b20e2607ce7c2dd45742ba7551e61d45c909cc29d9ef1e728ee842c"
  end
  if OS.linux? && Hardware::CPU.intel?
    url "https://github.com/dosco/graphjin/releases/download/v0.15.91/graphjin_0.15.91_Linux_x86_64.tar.gz"
    sha256 "19a26dbd0f52bf56454dd18206224164657b68702af8c090e133551ba0924de2"
  end
  if OS.linux? && Hardware::CPU.arm? && !Hardware::CPU.is_64_bit?
    url "https://github.com/dosco/graphjin/releases/download/v0.15.91/graphjin_0.15.91_Linux_armv6.tar.gz"
    sha256 "dc0912110dd1a72d6d7b258be63709834b2d3a8d48e31cce8dfbacd74e2e4bd0"
  end
  if OS.linux? && Hardware::CPU.arm? && Hardware::CPU.is_64_bit?
    url "https://github.com/dosco/graphjin/releases/download/v0.15.91/graphjin_0.15.91_Linux_arm64.tar.gz"
    sha256 "a1350564671461c0b2c42d4958d7907e7c5c99c44b8af8a6a9bb037d90d5b721"
  end

  def install
    bin.install "graphjin"
  end
end