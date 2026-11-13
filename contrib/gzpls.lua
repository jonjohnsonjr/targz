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

local function format_bytes(value)
  local units = { "B", "KiB", "MiB", "GiB", "TiB" }
  local amount = tonumber(value) or 0
  local unit = 1
  while amount >= 1024 and unit < #units do
    amount = amount / 1024
    unit = unit + 1
  end
  if unit == 1 then
    return string.format("%d %s", amount, units[unit])
  end
  return string.format("%.1f %s", amount, units[unit])
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
  vim.filetype.add({
    extension = { gzsym = "gzpls-symbols" },
  })
  vim.api.nvim_create_autocmd("FileType", {
    group = group,
    pattern = "gzpls-symbols",
    callback = function()
      vim.opt_local.number = true
      vim.opt_local.relativenumber = false
      -- Generated symbol documents are zero-based address spaces: the gutter
      -- value is the literal/length symbol, distance symbol, or program slot.
      vim.opt_local.statuscolumn = "%=%{v:lnum - 1} "
    end,
  })
  local jobs = {}
  local lsp_config = {
    name = "gzpls",
    cmd = command(cmd, "serve"),
    root_dir = root_dir,
    filetypes = { "gzpls", "gzpls-symbols" },
  }
  local definition_ns = vim.api.nvim_create_namespace("gzpls-definition")

  local function show_definition(options)
    if #options.items ~= 1 then
      vim.fn.setqflist({}, " ", options)
      vim.cmd("botright copen")
      return
    end

    local item = options.items[1]
    local buf = item.bufnr or vim.fn.bufadd(item.filename)
    vim.fn.bufload(buf)
    vim.cmd("normal! m'")
    vim.bo[buf].buflisted = true
    vim.api.nvim_win_set_buf(0, buf)
    local row, col = item.lnum - 1, item.col - 1
    vim.api.nvim_win_set_cursor(0, { row + 1, col })
    vim.cmd("normal! zv")

    local end_row = (item.end_lnum or item.lnum) - 1
    local end_col = (item.end_col or item.col) - 1
    vim.api.nvim_buf_clear_namespace(buf, definition_ns, 0, -1)
    if end_row > row or end_col > col then
      vim.api.nvim_buf_set_extmark(buf, definition_ns, row, col, {
        end_row = end_row,
        end_col = end_col,
        hl_group = "Visual",
        priority = 200,
      })
      vim.defer_fn(function()
        if vim.api.nvim_buf_is_valid(buf) then
          vim.api.nvim_buf_clear_namespace(buf, definition_ns, 0, -1)
        end
      end, 1500)
    end
  end

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
    vim.keymap.set("n", "gd", function()
      vim.lsp.buf.definition({ on_list = show_definition })
    end, map_opts)
    vim.keymap.set("n", "gri", vim.lsp.buf.implementation, map_opts)
    vim.keymap.set("n", "grr", vim.lsp.buf.references, map_opts)
    vim.keymap.set("n", "K", vim.lsp.buf.hover, map_opts)
  end

  local spinner = { "⣋", "⣙", "⣹", "⣸", "⣼", "⣴", "⣦", "⣧", "⣇", "⣏" }
  local phase_names = {
    inspect = "inspecting stream",
    index = "indexing possible filesystem",
    inflate = "decoding content",
    render = "rendering document",
  }

  local function render_splash(buf, state)
    if not vim.api.nvim_buf_is_valid(buf) or jobs[buf] ~= state then
      return
    end
    state.frame = state.frame % #spinner + 1
    local facts = {}
    if (state.compressed or 0) > 0 then
      table.insert(facts, format_bytes(state.compressed) .. " compressed")
    end
    if (state.decoded or 0) > 0 then
      table.insert(facts, format_bytes(state.decoded) .. " decoded")
    end
    if (state.entries or 0) > 0 then
      table.insert(facts, string.format("%d entries", state.entries))
    end
    local elapsed = (vim.uv.hrtime() - state.started) / 1e9
    local detail = state.detail or phase_names[state.phase] or "working"
    local lines = {
      "gzpls",
      "",
      string.format("%s %s · %.1fs", spinner[state.frame], detail, elapsed),
      table.concat(facts, " · "),
      "",
      "inflating expectations",
    }
    vim.bo[buf].readonly = false
    vim.bo[buf].modifiable = true
    vim.api.nvim_buf_set_lines(buf, 0, -1, false, lines)
    vim.bo[buf].modified = false
    vim.bo[buf].modifiable = false
    vim.bo[buf].readonly = true
  end

  local function consume_progress(state, chunk)
    state.progress_tail = state.progress_tail .. chunk
    while true do
      local newline = state.progress_tail:find("\n", 1, true)
      if not newline then
        return
      end
      local line = state.progress_tail:sub(1, newline - 1)
      state.progress_tail = state.progress_tail:sub(newline + 1)
      local ok, event = pcall(vim.json.decode, line)
      if ok and type(event) == "table" and event.phase then
        state.phase = event.phase
        state.detail = event.detail
        state.compressed = event.compressed or state.compressed
        state.decoded = event.decoded or state.decoded
        state.entries = event.entries or state.entries
      end
    end
  end

  vim.api.nvim_create_autocmd("BufReadCmd", {
    group = group,
    pattern = patterns,
    callback = function(event)
      local path = vim.api.nvim_buf_get_name(event.buf)
      local state = {
        started = vim.uv.hrtime(), frame = 1, phase = "inspect",
        output = {}, errors = {}, progress_tail = "",
      }
      jobs[event.buf] = state
      vim.bo[event.buf].readonly = true
      render_splash(event.buf, state)
      state.timer = vim.uv.new_timer()
      state.timer:start(100, 100, vim.schedule_wrap(function()
        render_splash(event.buf, state)
      end))

      state.process = vim.system(command(cmd, "disassemble", "-progress", "-view", "auto", path), {
        cwd = root_dir,
        text = true,
        stdout = function(err, data)
          if err then
            table.insert(state.errors, tostring(err))
          end
          if data then
            table.insert(state.output, data)
          end
        end,
        stderr = function(err, data)
          if err then
            table.insert(state.errors, tostring(err))
          end
          if data then
            table.insert(state.errors, data)
            vim.schedule(function()
              if jobs[event.buf] == state then
                consume_progress(state, data)
                render_splash(event.buf, state)
              end
            end)
          end
        end,
      }, function(result)
        vim.schedule(function()
          if state.timer then
            state.timer:stop()
            state.timer:close()
            state.timer = nil
          end
          if jobs[event.buf] ~= state or not vim.api.nvim_buf_is_valid(event.buf) then
            return
          end
          jobs[event.buf] = nil
          if result.code ~= 0 then
            vim.bo[event.buf].readonly = false
            vim.bo[event.buf].modifiable = true
            vim.api.nvim_buf_set_lines(event.buf, 0, -1, false, {
              "gzpls", "", "failed to inspect stream", "", table.concat(state.errors),
            })
            vim.bo[event.buf].modified = false
            vim.bo[event.buf].modifiable = false
            vim.bo[event.buf].readonly = true
            vim.notify("gzpls disassemble failed", vim.log.levels.ERROR)
            return
          end

          local output = table.concat(state.output)
          vim.bo[event.buf].readonly = false
          vim.bo[event.buf].modifiable = true
          vim.api.nvim_buf_set_lines(event.buf, 0, -1, false, split_lines(output))
          if output:find("# gzpls ", 1, true) then
            vim.bo[event.buf].filetype = "gzpls"
          elseif output:match("^%x%x%x%x%x%x%x%x: ") then
            vim.bo[event.buf].filetype = "xxd"
          else
            local decoded_name = path:gsub("%.gz$", "")
            vim.bo[event.buf].filetype = vim.filetype.match({ filename = decoded_name, buf = event.buf }) or ""
          end
          vim.bo[event.buf].modified = false
          vim.bo[event.buf].readonly = true
          attach(event.buf)
        end)
      end)
    end,
  })

  vim.api.nvim_create_autocmd("BufWipeout", {
    group = group,
    callback = function(event)
      local state = jobs[event.buf]
      if not state then
        return
      end
      jobs[event.buf] = nil
      if state.timer then
        state.timer:stop()
        state.timer:close()
      end
      if state.process then
        pcall(state.process.kill, state.process, 15)
      end
    end,
  })

  -- Definition/implementation targets are ordinary temporary files. Attach the
  -- existing client without changing their useful source-language filetype.
  vim.api.nvim_create_autocmd("BufReadPost", {
    group = group,
    callback = function(event)
      local name = vim.api.nvim_buf_get_name(event.buf)
      if name:find("/gzpls%-generated%-") then
        if name:match("%.gzsym$") then
          vim.bo[event.buf].filetype = "gzpls-symbols"
        end
        attach(event.buf)
      end
    end,
  })
end

return M
