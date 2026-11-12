-- Minimal Neovim integration for gzpls. This is deliberately a config file,
-- not a plugin: source it once from init.lua and it only takes over gzip/tar
-- document types.
local M = {}

local function command(base, ...)
  local result = vim.deepcopy(base)
  vim.list_extend(result, { ... })
  return result
end

local function split_lines(text)
  local lines = vim.split(text, "\n", { plain = true })
  if lines[#lines] == "" then
    table.remove(lines)
  end
  return lines
end

function M.setup(opts)
  opts = opts or {}
  local cmd = opts.cmd or { "gzpls" }
  local root_dir = opts.root_dir or vim.fn.getcwd()
  local patterns = opts.patterns or { "*.gz", "*.tgz" }

  -- Neovim's stock gzip/tar handlers eagerly replace these buffers with their
  -- own decoded/archive views. gzpls needs to see the compressed source, so it
  -- owns these document types after setup, even when setup runs after startup.
  vim.g.loaded_gzip = 1
  vim.g.loaded_tar = 1
  vim.g.loaded_tarPlugin = 1
  pcall(vim.api.nvim_del_augroup_by_name, "gzip")
  pcall(vim.api.nvim_del_augroup_by_name, "tar")

  local group = vim.api.nvim_create_augroup("gzpls", { clear = true })
  local lsp_config = {
    name = "gzpls",
    cmd = command(cmd, "serve"),
    root_dir = root_dir,
  }

  local function attach(buf)
    if not vim.api.nvim_buf_is_valid(buf) then
      return
    end
    local client_id = vim.lsp.start(lsp_config, { bufnr = buf })
    if not client_id then
      vim.notify("gzpls failed to start", vim.log.levels.ERROR)
      return
    end

    local map_opts = { buffer = buf, silent = true }
    vim.keymap.set("n", "gd", vim.lsp.buf.definition, map_opts)
    vim.keymap.set("n", "gri", vim.lsp.buf.implementation, map_opts)
    vim.keymap.set("n", "grr", vim.lsp.buf.references, map_opts)
    vim.keymap.set("n", "K", vim.lsp.buf.hover, map_opts)
  end

  vim.api.nvim_create_autocmd("BufReadCmd", {
    group = group,
    pattern = patterns,
    callback = function(event)
      local path = vim.api.nvim_buf_get_name(event.buf)
      local result = vim.system(command(cmd, "disassemble", "-view", "auto", path), {
        cwd = root_dir,
        text = true,
      }):wait()
      if result.code ~= 0 then
        error((result.stderr ~= "" and result.stderr) or "gzpls disassemble failed")
      end

      vim.bo[event.buf].modifiable = true
      vim.api.nvim_buf_set_lines(event.buf, 0, -1, false, split_lines(result.stdout))
      vim.bo[event.buf].filetype = "gzpls"
      vim.bo[event.buf].modified = false
      vim.bo[event.buf].readonly = true
      attach(event.buf)
    end,
  })

  -- Definition/implementation targets are ordinary temporary files. Attach the
  -- existing client without changing their useful source-language filetype.
  vim.api.nvim_create_autocmd("BufReadPost", {
    group = group,
    callback = function(event)
      local name = vim.api.nvim_buf_get_name(event.buf)
      if name:find("/gzpls%-generated%-") then
        attach(event.buf)
      end
    end,
  })
end

return M
